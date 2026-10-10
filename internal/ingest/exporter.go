package ingest

import (
	"context"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/exporter"
	"go.opentelemetry.io/collector/exporter/exporterhelper"
	"go.opentelemetry.io/collector/exporter/exporterhelper/xexporterhelper"
	"go.opentelemetry.io/collector/exporter/xexporter"
	"go.opentelemetry.io/collector/pdata/pprofile"

	"github.com/adrijh/kenbun/internal/store"
)

type Config struct{}

type kenbunExporter struct {
	store *store.Store
}

func (e *kenbunExporter) pushProfiles(ctx context.Context, pd pprofile.Profiles) error {
	return e.store.Ingest(ctx, Convert(pd))
}

// NewFactory returns the "kenbun" exporter factory. main owns st and its lifecycle.
func NewFactory(st *store.Store) exporter.Factory {
	return xexporter.NewFactory(
		component.MustNewType("kenbun"),
		func() component.Config { return &Config{} },
		xexporter.WithProfiles(
			func(ctx context.Context, set exporter.Settings, cfg component.Config) (xexporter.Profiles, error) {
				e := &kenbunExporter{store: st}
				return xexporterhelper.NewProfiles(ctx, set, cfg, e.pushProfiles,
					exporterhelper.WithCapabilities(consumer.Capabilities{MutatesData: false}),
				)
			},
			component.StabilityLevelDevelopment,
		),
	)
}
