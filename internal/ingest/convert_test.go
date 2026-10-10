package ingest

import (
	"slices"
	"testing"
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
