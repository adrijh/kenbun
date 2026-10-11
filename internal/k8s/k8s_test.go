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

func TestCompare(t *testing.T) {
	const repo = "ghcr.io/adrijh/kenbun:"
	for _, tc := range []struct {
		a, b string
		cmp  int
		ok   bool
	}{
		{repo + "v0.0.1", repo + "v0.0.1", 0, true},
		{repo + "v0.0.1", repo + "v0.0.2", -1, true},
		{repo + "v0.10.0", repo + "v0.9.3", 1, true},
		{repo + "v0.0.1-3-gabc", repo + "v0.0.1", 0, false}, // dev build
		{"other/kenbun:v0.0.1", repo + "v0.0.2", 0, false},
	} {
		if cmp, ok := Compare(tc.a, tc.b); cmp != tc.cmp || ok != tc.ok {
			t.Errorf("Compare(%s, %s) = %d, %v; want %d, %v", tc.a, tc.b, cmp, ok, tc.cmp, tc.ok)
		}
	}
}
