package k8s

import (
	"strings"
	"testing"
)

func TestManifests(t *testing.T) {
	m, err := Manifests(Options{Image: "example/kenbun:1.2.3", Profiler: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"kind: DaemonSet",
		"args: [agent,",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("manifests lack %q", want)
		}
	}
	if n := strings.Count(m, "image: example/kenbun:1.2.3"); n != 2 { // kenbun and the agent
		t.Errorf("kenbun image used %d times, want 2", n)
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
