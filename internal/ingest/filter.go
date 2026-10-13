package ingest

import (
	"context"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/consumer/xconsumer"
	"go.opentelemetry.io/collector/pdata/pprofile"
	"go.opentelemetry.io/collector/processor"
	"go.opentelemetry.io/collector/processor/processorhelper/xprocessorhelper"
	"go.opentelemetry.io/collector/processor/xprocessor"

	"github.com/adrijh/kenbun/internal/store"
)

// DropOffCPU removes the off-CPU samples Convert would drop: those outside an
// asyncio loop. Machine-wide they're nearly all of a batch's samples, so
// `kenbun agent` drops them before sending. The dictionary is left as is.
func DropOffCPU(pd pprofile.Profiles) {
	r := newResolver(pd.Dictionary())
	inLoop := map[int32]bool{} // by stack index
	rps := pd.ResourceProfiles()
	for i := 0; i < rps.Len(); i++ {
		sps := rps.At(i).ScopeProfiles()
		for j := 0; j < sps.Len(); j++ {
			profs := sps.At(j).Profiles()
			for k := 0; k < profs.Len(); k++ {
				prof := profs.At(k)
				if r.str(prof.SampleType().TypeStrindex()) != "off_cpu" {
					continue
				}
				prof.Samples().RemoveIf(func(s pprofile.Sample) bool {
					in, ok := inLoop[s.StackIndex()]
					if !ok {
						in = r.inLoop(s.StackIndex())
						inLoop[s.StackIndex()] = in
					}
					return !in
				})
			}
		}
	}
}

// inLoop reports whether a stack has the asyncio loop's frame. It's a Python
// frame, named in the dictionary: no native symbols needed.
func (r *resolver) inLoop(stackIdx int32) bool {
	stacks, locations, fns := r.d.StackTable(), r.d.LocationTable(), r.d.FunctionTable()
	if stackIdx <= 0 || int(stackIdx) >= stacks.Len() {
		return false
	}
	locIdx := stacks.At(int(stackIdx)).LocationIndices()
	for i := 0; i < locIdx.Len(); i++ {
		li := int(locIdx.At(i))
		if li <= 0 || li >= locations.Len() {
			continue
		}
		lines := locations.At(li).Lines()
		for j := 0; j < lines.Len(); j++ {
			if fi := int(lines.At(j).FunctionIndex()); fi > 0 && fi < fns.Len() && r.str(fns.At(fi).NameStrindex()) == store.LoopFrame {
				return true
			}
		}
	}
	return false
}

// NewFilterFactory returns the "kenbun_offcpu" processor, which runs DropOffCPU.
func NewFilterFactory() processor.Factory {
	return xprocessor.NewFactory(
		component.MustNewType("kenbun_offcpu"),
		func() component.Config { return &Config{} },
		xprocessor.WithProfiles(
			func(ctx context.Context, set processor.Settings, cfg component.Config, next xconsumer.Profiles) (xprocessor.Profiles, error) {
				return xprocessorhelper.NewProfiles(ctx, set, cfg, next,
					func(_ context.Context, pd pprofile.Profiles) (pprofile.Profiles, error) {
						DropOffCPU(pd)
						return pd, nil
					},
					xprocessorhelper.WithCapabilities(consumer.Capabilities{MutatesData: true}),
				)
			},
			component.StabilityLevelDevelopment,
		),
	)
}
