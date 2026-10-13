package main

import (
	"context"
	"runtime"
	"testing"
)

// The embedded collector configs only fail when kenbun starts, so validate them.
func TestCollectorConfigs(t *testing.T) {
	agent := append([]string{"yaml:" + agentConfig}, profilerConfig...)
	serve, err := localConfig("0.0.0.0:4317", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	configs := map[string][]string{"serve": serve}
	if runtime.GOOS == "linux" { // the profiler's components only exist there
		configs["agent"] = agent
		t.Setenv("KUBE_NODE_NAME", "node") // the DaemonSet sets it
	}
	for name, uris := range configs {
		col, err := newCollector(nil, uris)
		if err != nil {
			t.Fatal(err)
		}
		if err := col.DryRun(context.Background()); err != nil {
			t.Errorf("%s config: %v", name, err)
		}
	}
}
