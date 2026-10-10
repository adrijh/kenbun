//go:build !linux

package main

import (
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/extension"
)

// extensionFactories: none; --profile and the agent are Linux only.
func extensionFactories() (map[component.Type]extension.Factory, error) {
	return nil, nil
}
