package k8s

import (
	"strings"
	"testing"

	_ "go.opentelemetry.io/collector/otelcol" // puts its version in build info, as in the kenbun binary
)

func TestManifests(t *testing.T) {
	m, err := Manifests(Options{Image: "example/kenbun:1.2.3", Profiler: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"image: example/kenbun:1.2.3",
		"otel/opentelemetry-collector-ebpf-profiler:0.162.0", // go.mod's otelcol version
		"kind: DaemonSet",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("manifests lack %q", want)
		}
	}
}

func TestManifestsWithoutProfiler(t *testing.T) {
	m, err := Manifests(Options{Image: "kenbun:dev"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(m, "DaemonSet") || !strings.Contains(m, "kind: Deployment") {
		t.Error("want kenbun only")
	}
}
