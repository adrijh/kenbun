package ingest

import (
	"path"
	"slices"
	"strconv"
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
					frames, locs := r.frames(smp.StackIndex(), pid)
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
						Locs:    locs,
						Weight:  weight(smp),
					})
				}
			}
		}
	}
	return out
}

// processKey prefers service.name (set by SDKs or the k8s_attributes processor)
// and falls back to the executable name the eBPF profiler sets. In k8s it adds
// the container: k8s_attributes takes service.name from app.kubernetes.io/instance,
// which Helm sets to the release name on every component of a chart.
func processKey(attrs pcommon.Map) string {
	s := strAttr(attrs, "service.name")
	if s == "" {
		return strAttr(attrs, "process.executable.name")
	}
	if c := strAttr(attrs, "k8s.container.name"); c != "" && c != s {
		return s + "/" + c
	}
	return s
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

// resolver turns stack indices into frames, caching per batch: many samples
// in a batch share the same stack.
type resolver struct {
	d     pprofile.ProfilesDictionary
	cache map[int32]stack
}

type stack struct{ frames, locs []string }

func newResolver(d pprofile.ProfilesDictionary) *resolver {
	return &resolver{d: d, cache: map[int32]stack{}}
}

// frames returns function names and their file:line, leaf first. Native
// functions get their library instead of a file:line. Index 0 in every
// dictionary table is an empty placeholder, so it's treated as "missing".
// pid only helps find native binaries: stacks are shared by processes.
func (r *resolver) frames(stackIdx int32, pid int64) (frames, locs []string) {
	if s, ok := r.cache[stackIdx]; ok {
		return s.frames, s.locs
	}

	stacks := r.d.StackTable()
	if stackIdx <= 0 || int(stackIdx) >= stacks.Len() {
		return nil, nil
	}
	locations, fns, maps := r.d.LocationTable(), r.d.FunctionTable(), r.d.MappingTable()

	locIdx := stacks.At(int(stackIdx)).LocationIndices()
	frames, locs = make([]string, 0, locIdx.Len()), make([]string, 0, locIdx.Len())
	for i := 0; i < locIdx.Len(); i++ {
		li := int(locIdx.At(i))
		if li <= 0 || li >= locations.Len() {
			continue
		}
		loc := locations.At(li)
		lines := loc.Lines()

		if lines.Len() == 0 {
			// Native frame: name it from the binary's symbols if this machine
			// has them (symbols.go). Otherwise label it with its library and
			// collapse runs (a Python stack starts with ~12 libpython frames).
			name := "[??]"
			if mi := int(loc.MappingIndex()); mi > 0 && mi < maps.Len() {
				m := maps.At(mi)
				if f := r.str(m.FilenameStrindex()); f != "" {
					if fn := symbolize(r.attr(m.AttributeIndices(), "process.executable.build_id.gnu"), f, pid, loc.Address()); fn != "" {
						frames, locs = append(frames, fn), append(locs, path.Base(f))
						continue
					}
					name = "[" + path.Base(f) + "]"
				}
			}
			if n := len(frames); n > 0 && frames[n-1] == name {
				continue
			}
			frames, locs = append(frames, name), append(locs, "")
			continue
		}

		// Inlined frames: Lines()[0] is the innermost.
		for j := 0; j < lines.Len(); j++ {
			name, where := "??", ""
			if fi := int(lines.At(j).FunctionIndex()); fi > 0 && fi < fns.Len() {
				fn := fns.At(fi)
				if n := r.str(fn.NameStrindex()); n != "" {
					name = n
				}
				// Only with a line: some native frames carry a bogus file.
				if f, l := r.str(fn.FilenameStrindex()), lines.At(j).Line(); f != "" && l > 0 {
					where = f + ":" + strconv.FormatInt(l, 10)
				}
			}
			frames, locs = append(frames, name), append(locs, where)
		}
	}

	r.cache[stackIdx] = stack{frames, locs}
	return frames, locs
}

// attr returns the string attribute key among the dictionary attributes at idx.
func (r *resolver) attr(idx pcommon.Int32Slice, key string) string {
	attrs := r.d.AttributeTable()
	for i := 0; i < idx.Len(); i++ {
		if ai := int(idx.At(i)); ai > 0 && ai < attrs.Len() && r.str(attrs.At(ai).KeyStrindex()) == key {
			return attrs.At(ai).Value().AsString()
		}
	}
	return ""
}

// thread returns the sample's thread.name (the kernel comm), or "".
func (r *resolver) thread(s pprofile.Sample) string {
	return r.attr(s.AttributeIndices(), "thread.name")
}

func (r *resolver) str(i int32) string {
	st := r.d.StringTable()
	if i <= 0 || int(i) >= st.Len() {
		return ""
	}
	return st.At(int(i))
}
