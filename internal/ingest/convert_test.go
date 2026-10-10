package ingest

import (
	"slices"
	"testing"

	"go.opentelemetry.io/collector/pdata/pcommon"
)

// testdata/profiles.jsonl is a file-exporter capture of the demo app (HOT=fib, python3.14).
// Expected numbers come from inspect_profiles.py on the same file.
func TestConvertFixture(t *testing.T) {
	samples, err := ReadJSONL("testdata/profiles.jsonl")
	if err != nil {
		t.Fatal(err)
	}

	byProcess := map[string]int64{}
	fibTotal := int64(0)
	for _, s := range samples {
		byProcess[s.Process] += s.Weight
		if s.Process == "python3.14" && slices.Contains(s.Frames, "fib") {
			fibTotal += s.Weight
		}
	}

	py := byProcess["python3.14"]
	if py != 657 {
		t.Fatalf("python3.14 weight = %d, want 657 (processes seen: %v)", py, byProcess)
	}
	if pct := 100 * float64(fibTotal) / float64(py); pct < 37 || pct > 38.5 {
		t.Errorf("fib total = %.1f%%, want ~37.7%%", pct)
	}
	for _, skipped := range []string{"otelcol-ebpf-profiler", "kenbun"} {
		if byProcess[skipped] != 0 {
			t.Errorf("%s should be skipped", skipped)
		}
	}
}

func TestProcessKey(t *testing.T) {
	for _, tc := range []struct {
		attrs map[string]any
		want  string
	}{
		{map[string]any{"process.executable.name": "python3.14"}, "python3.14"},
		{map[string]any{"service.name": "api", "process.executable.name": "python3.14"}, "api"},
		// Helm: every component of the argo-cd release gets service.name argo-cd.
		{map[string]any{"service.name": "argo-cd", "k8s.container.name": "redis"}, "argo-cd/redis"},
		{map[string]any{"service.name": "api", "k8s.container.name": "api"}, "api"},
	} {
		attrs := pcommon.NewMap()
		if err := attrs.FromRaw(tc.attrs); err != nil {
			t.Fatal(err)
		}
		if got := processKey(attrs); got != tc.want {
			t.Errorf("processKey(%v) = %q, want %q", tc.attrs, got, tc.want)
		}
	}
}
