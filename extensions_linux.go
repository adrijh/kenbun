package main

import (
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/extension"
	"go.opentelemetry.io/collector/otelcol"
	"go.opentelemetry.io/ebpf-profiler/probes/offcpu"
)

// extensionFactories: the off-CPU probe only builds on Linux.
func extensionFactories() (map[component.Type]extension.Factory, error) {
	return otelcol.MakeFactoryMap[extension.Factory](offcpu.NewFactory())
}
