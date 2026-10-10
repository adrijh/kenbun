// kenbun receives OTLP profiles through an embedded OpenTelemetry Collector
// and shows them in a TUI.
package main

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/open-telemetry/opentelemetry-collector-contrib/exporter/fileexporter"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/confmap"
	"go.opentelemetry.io/collector/confmap/provider/yamlprovider"
	"go.opentelemetry.io/collector/featuregate"
	"go.opentelemetry.io/collector/otelcol"
	"go.opentelemetry.io/collector/receiver/otlpreceiver"
	"go.opentelemetry.io/collector/service/telemetry/otelconftelemetry"

	"github.com/adrijh/kenbun/internal/api"
	"github.com/adrijh/kenbun/internal/ingest"
	"github.com/adrijh/kenbun/internal/store"
	"github.com/adrijh/kenbun/internal/store/sqlite"
	"github.com/adrijh/kenbun/internal/tui"
)

//go:embed otel.yaml
var baseConfig string

const (
	version   = "0.3.0"
	retention = 30 * time.Minute // largest TUI window (15m) plus its diff baseline
)

const usage = `kenbun: OTLP profiles in your terminal

  kenbun [--otlp addr] [--dump file.jsonl]   receive profiles and show the TUI
  kenbun view file.jsonl                     show a capture (from --dump) offline
  kenbun serve [--otlp addr] [--http addr]   receive profiles and serve the HTTP API
  kenbun tui [--addr host:port]              TUI for a remote 'kenbun serve'
`

func main() {
	cmd, args := "", os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "":
		err = runLocal(args)
	case "view":
		err = runView(args)
	case "serve":
		err = runServe(args)
	case "tui":
		err = runRemote(args)
	default:
		err = fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
	}
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, "kenbun:", err)
		os.Exit(1)
	}
}

func flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), usage+"\nflags:\n")
		fs.PrintDefaults()
	}
	return fs
}

func runLocal(args []string) error {
	fs := flags("kenbun")
	otlp := fs.String("otlp", "localhost:4317", "OTLP gRPC listen address")
	dump := fs.String("dump", "", "also write received profiles to this JSONL file")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// The TUI owns the terminal, so logs (ours and the collector's) go to a file.
	logPath, err := logFile()
	if err != nil {
		return err
	}
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer lf.Close()
	log.SetOutput(lf)

	st, err := sqlite.Open("")
	if err != nil {
		return err
	}
	defer st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go prune(ctx, st)

	col, err := newCollector(st, *otlp, *dump, logPath)
	if err != nil {
		return err
	}
	if err := runTUI(tui.New(st, tui.Options{}), col); err != nil {
		return fmt.Errorf("%w (log: %s)", err, logPath)
	}
	return nil
}

func runView(args []string) error {
	fs := flags("view")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("view takes one file\n\n%s", usage)
	}
	samples, err := ingest.ReadJSONL(fs.Arg(0))
	if err != nil {
		return err
	}
	if len(samples) == 0 {
		return fmt.Errorf("%s: no samples", fs.Arg(0))
	}
	st, err := sqlite.Open("")
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Ingest(context.Background(), samples); err != nil {
		return err
	}

	first, last := samples[0].TS, samples[0].TS
	for _, s := range samples {
		first, last = minTime(first, s.TS), maxTime(last, s.TS)
	}
	log.SetOutput(io.Discard)
	return runTUI(tui.New(st, tui.Options{At: last.Add(time.Nanosecond), Window: last.Sub(first)}), nil)
}

func runServe(args []string) error {
	fs := flags("serve")
	otlp := fs.String("otlp", "0.0.0.0:4317", "OTLP gRPC listen address")
	httpAddr := fs.String("http", ":7070", "HTTP API listen address")
	dump := fs.String("dump", "", "also write received profiles to this JSONL file")
	if err := fs.Parse(args); err != nil {
		return err
	}

	st, err := sqlite.Open("")
	if err != nil {
		return err
	}
	defer st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go prune(ctx, st)

	col, err := newCollector(st, *otlp, *dump, "") // logs to stderr
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", *httpAddr) // fail before starting the collector
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: api.Handler(st), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			log.Printf("http: %v", err)
			col.Shutdown()
		}
	}()
	log.Printf("API on %s, OTLP on %s", ln.Addr(), *otlp)

	err = col.Run(ctx) // returns on SIGINT/SIGTERM
	shutdownCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	return errors.Join(err, srv.Shutdown(shutdownCtx))
}

func runRemote(args []string) error {
	fs := flags("tui")
	addr := fs.String("addr", "localhost:7070", "address of 'kenbun serve'")
	if err := fs.Parse(args); err != nil {
		return err
	}
	log.SetOutput(io.Discard)
	return runTUI(tui.New(api.NewClient(*addr), tui.Options{}), nil)
}

// runTUI runs the TUI and, if given, the collector alongside it. The collector
// stops when the TUI quits; if it stops first (bad config, port in use,
// SIGTERM), it takes the TUI down with it.
func runTUI(m tea.Model, col *otelcol.Collector) error {
	p := tea.NewProgram(m)

	// The collector traps SIGHUP (config reload), so without this closing the
	// terminal would leave kenbun running headless and holding the OTLP port.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() { <-hup; p.Quit() }()

	colErr := make(chan error, 1)
	if col != nil {
		go func() {
			colErr <- col.Run(context.Background())
			p.Quit()
		}()
	} else {
		colErr <- nil
	}

	_, err := p.Run()
	if col != nil {
		col.Shutdown()
	}
	if cerr := <-colErr; cerr != nil {
		return fmt.Errorf("collector: %w", cerr)
	}
	return err
}

func newCollector(st *store.Store, otlp, dump, logPath string) (*otelcol.Collector, error) {
	if err := featuregate.GlobalRegistry().Set("service.profilesSupport", true); err != nil {
		return nil, err
	}

	// Later URIs override earlier ones; lists are replaced, not merged.
	uris := []string{
		"yaml:" + baseConfig,
		"yaml:receivers::otlp::protocols::grpc::endpoint: " + strconv.Quote(otlp),
	}
	if dump != "" {
		if err := os.MkdirAll(filepath.Dir(dump), 0o755); err != nil {
			return nil, err
		}
		uris = append(uris,
			"yaml:exporters::file/dump::path: "+strconv.Quote(dump),
			"yaml:service::pipelines::profiles::exporters: [kenbun, file/dump]",
		)
	}
	if logPath != "" {
		q := "[" + strconv.Quote(logPath) + "]"
		uris = append(uris,
			"yaml:service::telemetry::logs::output_paths: "+q,
			"yaml:service::telemetry::logs::error_output_paths: "+q,
		)
	}

	return otelcol.NewCollector(otelcol.CollectorSettings{
		BuildInfo: component.BuildInfo{
			Command:     "kenbun",
			Description: "OTLP profiles in your terminal",
			Version:     version,
		},
		Factories: func() (otelcol.Factories, error) {
			receivers, err := otelcol.MakeFactoryMap(otlpreceiver.NewFactory())
			if err != nil {
				return otelcol.Factories{}, err
			}
			exporters, err := otelcol.MakeFactoryMap(ingest.NewFactory(st), fileexporter.NewFactory())
			if err != nil {
				return otelcol.Factories{}, err
			}
			return otelcol.Factories{
				Receivers: receivers,
				Exporters: exporters,
				Telemetry: otelconftelemetry.NewFactory(),
			}, nil
		},
		ConfigProviderSettings: otelcol.ConfigProviderSettings{
			ResolverSettings: confmap.ResolverSettings{
				URIs:              uris,
				ProviderFactories: []confmap.ProviderFactory{yamlprovider.NewFactory()},
			},
		},
	})
}

// logFile is $XDG_STATE_HOME/kenbun/kenbun.log (default ~/.local/state).
func logFile() (string, error) {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".local", "state")
	}
	dir = filepath.Join(dir, "kenbun")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(dir, "kenbun.log"), nil
}

// prune enforces retention and refreshes planner stats (see store.Prune).
func prune(ctx context.Context, st *store.Store) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if err := st.Prune(ctx, now.Add(-retention)); err != nil {
				log.Printf("prune: %v", err)
			}
		}
	}
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
