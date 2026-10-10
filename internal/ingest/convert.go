package ingest

import (
	"path"
	"slices"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pprofile"

	"github.com/adrijh/kenbun/internal/store"
)

// skipExecutables are always profiled but never what you're looking for.
// Matched on the executable, since service.name may be set by k8s enrichment.
var skipExecutables = map[string]bool{
	"otelcol-ebpf-profiler": true,
	"kenbun":                true,
}

// Convert turns one OTLP batch into kenbun samples.
// It must not keep references to pd: everything is copied into plain Go values.
func Convert(pd pprofile.Profiles) []store.Sample {
	dict := pd.Dictionary()
	r := newResolver(dict)

	var out []store.Sample
	rps := pd.ResourceProfiles()
	for i := 0; i < rps.Len(); i++ {
		rp := rps.At(i)
		attrs := rp.Resource().Attributes()

		process := processKey(attrs)
		if process == "" || skipExecutables[strAttr(attrs, "process.executable.name")] {
			continue // kernel threads (no executable) and kenbun's own plumbing
		}
		pid := intAttr(attrs, "process.pid")

		sps := rp.ScopeProfiles()
		for j := 0; j < sps.Len(); j++ {
			profs := sps.At(j).Profiles()
			for k := 0; k < profs.Len(); k++ {
				prof := profs.At(k)
				fallbackTS := prof.Time().AsTime()
				// The off-CPU probe reports its own profile, valued in nanoseconds
				// blocked; everything else is on-CPU samples.
				offCPU := r.str(prof.SampleType().TypeStrindex()) == "off_cpu"

				samples := prof.Samples()
				for s := 0; s < samples.Len(); s++ {
					smp := samples.At(s)
					frames := r.frames(smp.StackIndex())
					// Only the LOOP view reads off-CPU time, and only inside an asyncio
					// loop. Machine-wide it's ~5x the on-CPU rows, so drop the rest.
					if offCPU && !slices.Contains(frames, store.LoopFrame) {
						continue
					}
					out = append(out, store.Sample{
						TS:      sampleTime(smp, fallbackTS),
						Process: process,
						PID:     pid,
						Thread:  r.thread(smp),
						OffCPU:  offCPU,
						Frames:  frames,
						Weight:  weight(smp),
					})
				}
			}
		}
	}
	return out
}

// processKey prefers service.name (set by SDKs or the k8s_attributes processor)
// and falls back to the executable name the eBPF profiler sets.
func processKey(attrs pcommon.Map) string {
	if s := strAttr(attrs, "service.name"); s != "" {
		return s
	}
	return strAttr(attrs, "process.executable.name")
}

func strAttr(attrs pcommon.Map, key string) string {
	if v, ok := attrs.Get(key); ok {
		return v.Str()
	}
	return ""
}

func intAttr(attrs pcommon.Map, key string) int64 {
	if v, ok := attrs.Get(key); ok && v.Type() == pcommon.ValueTypeInt {
		return v.Int()
	}
	return 0
}

// weight: the eBPF profiler leaves on-CPU Values empty and records one timestamp
// per event. Off-CPU Values are the nanoseconds of each event.
func weight(s pprofile.Sample) int64 {
	if v := s.Values(); v.Len() > 0 {
		var sum int64
		for i := 0; i < v.Len(); i++ {
			sum += v.At(i)
		}
		return sum
	}
	if n := s.TimestampsUnixNano().Len(); n > 0 {
		return int64(n)
	}
	return 1
}

func sampleTime(s pprofile.Sample, fallback time.Time) time.Time {
	if ts := s.TimestampsUnixNano(); ts.Len() > 0 {
		return time.Unix(0, int64(ts.At(0))).UTC()
	}
	return fallback.UTC()
}

// resolver turns stack indices into frame names, caching per batch:
// many samples in a batch share the same stack.
type resolver struct {
	d     pprofile.ProfilesDictionary
	cache map[int32][]string
}

func newResolver(d pprofile.ProfilesDictionary) *resolver {
	return &resolver{d: d, cache: map[int32][]string{}}
}

// frames returns function names, leaf first. Index 0 in every dictionary
// table is an empty placeholder, so it's treated as "missing".
func (r *resolver) frames(stackIdx int32) []string {
	if f, ok := r.cache[stackIdx]; ok {
		return f
	}

	stacks := r.d.StackTable()
	if stackIdx <= 0 || int(stackIdx) >= stacks.Len() {
		return nil
	}
	locs, fns, maps := r.d.LocationTable(), r.d.FunctionTable(), r.d.MappingTable()

	locIdx := stacks.At(int(stackIdx)).LocationIndices()
	out := make([]string, 0, locIdx.Len())
	for i := 0; i < locIdx.Len(); i++ {
		li := int(locIdx.At(i))
		if li <= 0 || li >= locs.Len() {
			continue
		}
		loc := locs.At(li)
		lines := loc.Lines()

		if lines.Len() == 0 {
			// Unsymbolized native frame: label it with its library and collapse
			// runs (a Python stack starts with ~12 libpython frames otherwise).
			name := "[??]"
			if mi := int(loc.MappingIndex()); mi > 0 && mi < maps.Len() {
				if f := r.str(maps.At(mi).FilenameStrindex()); f != "" {
					name = "[" + path.Base(f) + "]"
				}
			}
			if n := len(out); n > 0 && out[n-1] == name {
				continue
			}
			out = append(out, name)
			continue
		}

		// Inlined frames: Lines()[0] is the innermost.
		for j := 0; j < lines.Len(); j++ {
			name := "??"
			if fi := int(lines.At(j).FunctionIndex()); fi > 0 && fi < fns.Len() {
				if n := r.str(fns.At(fi).NameStrindex()); n != "" {
					name = n
				}
			}
			out = append(out, name)
		}
	}

	r.cache[stackIdx] = out
	return out
}

// thread returns the sample's thread.name (the kernel comm), or "".
func (r *resolver) thread(s pprofile.Sample) string {
	attrs := r.d.AttributeTable()
	idx := s.AttributeIndices()
	for i := 0; i < idx.Len(); i++ {
		if ai := int(idx.At(i)); ai > 0 && ai < attrs.Len() {
			if a := attrs.At(ai); r.str(a.KeyStrindex()) == "thread.name" {
				return a.Value().Str()
			}
		}
	}
	return ""
}

func (r *resolver) str(i int32) string {
	st := r.d.StringTable()
	if i <= 0 || int(i) >= st.Len() {
		return ""
	}
	return st.At(int(i))
}
