package ingest

import (
	"bufio"
	"compress/gzip"
	"os"
	"reflect"
	"testing"

	"go.opentelemetry.io/collector/pdata/pprofile"
)

// DropOffCPU keeps off-CPU samples in the loop and drops the rest; on-CPU ones
// stay wherever they are.
func TestDropOffCPU(t *testing.T) {
	pd := pprofile.NewProfiles()
	d := pd.Dictionary()
	str := func(v string) int32 { d.StringTable().Append(v); return int32(d.StringTable().Len() - 1) }
	str("")
	d.FunctionTable().AppendEmpty()
	d.LocationTable().AppendEmpty()
	d.StackTable().AppendEmpty()
	stack := func(names ...string) int32 { // leaf first
		li := d.StackTable().AppendEmpty().LocationIndices()
		for _, n := range names {
			d.FunctionTable().AppendEmpty().SetNameStrindex(str(n))
			l := d.LocationTable().AppendEmpty().Lines().AppendEmpty()
			l.SetFunctionIndex(int32(d.FunctionTable().Len() - 1))
			li.Append(int32(d.LocationTable().Len() - 1))
		}
		return int32(d.StackTable().Len() - 1)
	}
	inLoop := stack("do_nanosleep", "sleepy_handler", "BaseEventLoop._run_once", "<module>")
	outside := stack("do_epoll_wait", "main")

	rp := pd.ResourceProfiles().AppendEmpty()
	rp.Resource().Attributes().PutStr("process.executable.name", "python3")
	profs := rp.ScopeProfiles().AppendEmpty().Profiles()
	off := profs.AppendEmpty()
	off.SampleType().SetTypeStrindex(str("off_cpu"))
	for _, st := range []int32{inLoop, outside} {
		smp := off.Samples().AppendEmpty()
		smp.SetStackIndex(st)
		smp.Values().Append(1000)
	}
	on := profs.AppendEmpty()
	on.SampleType().SetTypeStrindex(str("samples"))
	on.Samples().AppendEmpty().SetStackIndex(outside)

	want := Convert(pd)
	DropOffCPU(pd)
	if off.Samples().Len() != 1 || off.Samples().At(0).StackIndex() != inLoop {
		t.Errorf("off-CPU kept %d samples, want the one in the loop", off.Samples().Len())
	}
	if on.Samples().Len() != 1 {
		t.Errorf("on-CPU kept %d samples, want 1", on.Samples().Len())
	}
	if got := Convert(pd); !reflect.DeepEqual(got, want) {
		t.Errorf("Convert after DropOffCPU = %+v, want %+v", got, want)
	}
}

// On a real capture DropOffCPU loses nothing kenbun reads.
func TestDropOffCPUFixture(t *testing.T) {
	f, err := os.Open("testdata/asyncio.jsonl.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(gz)
	sc.Buffer(nil, 64<<20)

	var u pprofile.JSONUnmarshaler
	for sc.Scan() {
		pd, err := u.UnmarshalProfiles(sc.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		want := Convert(pd)
		DropOffCPU(pd)
		if got := Convert(pd); !reflect.DeepEqual(got, want) {
			t.Fatalf("Convert after DropOffCPU differs: %d samples, want %d", len(got), len(want))
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
}
