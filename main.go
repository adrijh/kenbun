// kenbun receives OTLP profiles through an embedded OpenTelemetry Collector
// and shows them in a TUI.
package main

import (
	"cmp"
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
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/open-telemetry/opentelemetry-collector-contrib/exporter/fileexporter"
	"github.com/open-telemetry/opentelemetry-collector-contrib/processor/k8sattributesprocessor"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/confmap"
	"go.opentelemetry.io/collector/confmap/provider/yamlprovider"
	"go.opentelemetry.io/collector/exporter/otlpexporter"
	"go.opentelemetry.io/collector/featuregate"
	"go.opentelemetry.io/collector/otelcol"
	"go.opentelemetry.io/collector/receiver/otlpreceiver"
	"go.opentelemetry.io/collector/service/telemetry/otelconftelemetry"
	profiler "go.opentelemetry.io/ebpf-profiler/collector"

	"github.com/adrijh/kenbun/internal/api"
	"github.com/adrijh/kenbun/internal/cli"
	"github.com/adrijh/kenbun/internal/ingest"
	"github.com/adrijh/kenbun/internal/k8s"
	"github.com/adrijh/kenbun/internal/store"
	"github.com/adrijh/kenbun/internal/store/sqlite"
	"github.com/adrijh/kenbun/internal/tui"
)

//go:embed otel.yaml
var baseConfig string

//go:embed agent.yaml
var agentConfig string

// version is set by the Makefile (-ldflags "-X main.version=...") and is also
// the image tag `kenbun k8s deploy` uses by default.
var version = "dev"

const retention = 30 * time.Minute // largest TUI window (15m) plus its diff baseline

const usage = `kenbun: OTLP profiles in your terminal

  kenbun [tui] [--otlp addr] [--dump file.jsonl] [--profile]
                                             receive profiles and show the TUI; --profile also
                                             profiles this machine (eBPF: Linux, root)
  kenbun [tui] --addr host:port              TUI for a remote 'kenbun serve'
  kenbun view file.jsonl[.gz]                show a capture (from --dump) offline
  kenbun serve [--otlp addr] [--http addr] [--profile]
                                             receive profiles and serve the HTTP API

  kenbun top [--since 5m] [--limit 20] [--json] [--addr host:port]
                                             processes by CPU, from a 'kenbun serve'
  kenbun functions <process> [flags]         a process's hottest functions, self and total
  kenbun loop <process> [flags]              asyncio event loop: blocked %, callbacks cpu vs wait
  kenbun diff <process> [flags]              functions that changed vs the window before

  kenbun k8s [tui] [--context c]             port-forward to the cluster and open the TUI
  kenbun k8s top|functions|loop|diff [--context c] [flags]
                                             the queries above, on the cluster
  kenbun k8s deploy [--context c] [--image i] [--profiler=false]
                                             install kenbun + eBPF profiler (kubectl apply)
  kenbun k8s destroy [--context c]           remove everything deploy created
  kenbun k8s manifests [--image i] [--profiler=false]
                                             print the manifests instead
  kenbun agent [--to host:port]              eBPF profiler on a k8s node, pushing to kenbun
                                             (the DaemonSet 'k8s deploy' creates)
`

func main() {
	cmd, args := "", os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "", "tui":
		err = runLocal(args)
	case "view":
		err = runView(args)
	case "serve":
		err = runServe(args)
	case "top", "functions", "loop", "diff":
		err = runQuery(cmd, args)
	case "agent":
		err = runAgent(args)
	case "k8s":
		err = runK8s(args)
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
	profile := fs.Bool("profile", false, "also profile this machine with the embedded eBPF profiler (Linux, needs root)")
	addr := fs.String("addr", "", "show a remote 'kenbun serve' at host:port instead")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *addr != "" {
		log.SetOutput(io.Discard)
		return runTUI(tui.New(api.NewClient(*addr), tui.Options{}), nil)
	}
	if *profile && runtime.GOOS != "linux" {
		return errors.New("--profile uses eBPF and only works on Linux")
	}
	explicit := false
	fs.Visit(func(f *flag.Flag) { explicit = explicit || f.Name == "otlp" })
	if !explicit {
		*otlp = freeAddr(*otlp)
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

	uris, err := localConfig(*otlp, *dump, logPath, *profile)
	if err != nil {
		return err
	}
	col, err := newCollector(st, uris)
	if err != nil {
		return err
	}
	if err := runTUI(tui.New(st, tui.Options{OTLP: *otlp}), col); err != nil {
		if strings.Contains(err.Error(), `"profiling" receiver`) && os.Geteuid() != 0 {
			err = fmt.Errorf("%w\n--profile loads eBPF programs and needs root: sudo kenbun --profile", err)
		}
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
	profile := fs.Bool("profile", false, "also profile this machine with the embedded eBPF profiler (Linux, needs root)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *profile && runtime.GOOS != "linux" {
		return errors.New("--profile uses eBPF and only works on Linux")
	}

	st, err := sqlite.Open("")
	if err != nil {
		return err
	}
	defer st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go prune(ctx, st)

	uris, err := localConfig(*otlp, *dump, "", *profile) // logs to stderr
	if err != nil {
		return err
	}
	col, err := newCollector(st, uris)
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

// runQuery runs top, functions, loop or diff against a 'kenbun serve'.
func runQuery(cmd string, args []string) error {
	fs := flags(cmd)
	var q cli.Query
	queryFlags(fs, &q)
	addr := fs.String("addr", "localhost:7070", "address of 'kenbun serve'")
	proc, err := parseQuery(fs, cmd, args)
	if err != nil {
		return err
	}
	q.Src = api.NewClient(*addr)
	err = query(context.Background(), cmd, proc, q)
	if errors.Is(err, syscall.ECONNREFUSED) {
		err = fmt.Errorf("%w\nis 'kenbun serve' running there? (or 'kenbun k8s %s' for a cluster)", err, cmd)
	}
	return err
}

func queryFlags(fs *flag.FlagSet, q *cli.Query) {
	fs.DurationVar(&q.Since, "since", 5*time.Minute, "window: the last duration (30s, 5m, 1h)")
	fs.IntVar(&q.Limit, "limit", 20, "rows to show (0: all)")
	fs.BoolVar(&q.JSON, "json", false, "print JSON")
}

// parseQuery parses flags on either side of the process name, which every
// command but top takes: 'kenbun loop api --since 1m' works too.
func parseQuery(fs *flag.FlagSet, cmd string, args []string) (proc string, err error) {
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if cmd == "top" {
		if fs.NArg() > 0 {
			return "", fmt.Errorf("top takes no arguments, got %q", fs.Args())
		}
		return "", nil
	}
	if fs.NArg() == 0 {
		return "", fmt.Errorf("%s needs a process name: see 'kenbun top'", cmd)
	}
	proc = fs.Arg(0)
	if err := fs.Parse(fs.Args()[1:]); err != nil {
		return "", err
	}
	if fs.NArg() > 0 {
		return "", fmt.Errorf("%s takes one process name, got extra %q", cmd, fs.Args())
	}
	return proc, nil
}

func query(ctx context.Context, cmd, proc string, q cli.Query) error {
	q.Out = os.Stdout
	switch cmd {
	case "top":
		return cli.Top(ctx, q)
	case "functions":
		return cli.Functions(ctx, q, proc)
	case "loop":
		return cli.Loop(ctx, q, proc)
	default:
		return cli.Diff(ctx, q, proc)
	}
}

func runAgent(args []string) error {
	fs := flags("agent")
	to := fs.String("to", "kenbun.kenbun.svc:4317", "kenbun's OTLP gRPC address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if runtime.GOOS != "linux" {
		return errors.New("agent uses eBPF and only works on Linux")
	}
	uris := append([]string{"yaml:" + agentConfig}, profilerConfig...)
	uris = append(uris, "yaml:exporters::otlp_grpc::endpoint: "+strconv.Quote(*to))
	col, err := newCollector(nil, uris) // no store: the agent only forwards
	if err != nil {
		return err
	}
	return col.Run(context.Background()) // returns on SIGINT/SIGTERM
}

func runK8s(args []string) error {
	sub := "tui"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	fs := flags("k8s " + sub)
	var o k8s.Options
	fs.StringVar(&o.Context, "context", "", "kubeconfig context (default: current)")
	ctx := context.Background()

	switch sub {
	case "top", "functions", "loop", "diff":
		var q cli.Query
		queryFlags(fs, &q)
		proc, err := parseQuery(fs, sub, args)
		if err != nil {
			return err
		}
		addr, stop, err := k8s.PortForward(ctx, o)
		if err != nil {
			return err
		}
		defer stop()
		q.Src = api.NewClient(addr)
		return query(ctx, sub, proc, q)
	}

	fs.StringVar(&o.Image, "image", "ghcr.io/adrijh/kenbun:"+version, "kenbun image")
	fs.BoolVar(&o.Profiler, "profiler", true, "deploy the eBPF profiler DaemonSet (needs nodes in the host PID namespace: not k3d or kind)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	switch sub {
	case "deploy":
		if err := k8s.Deploy(ctx, o); err != nil {
			return err
		}
		fmt.Println("\nkenbun is running. Open it with: kenbun k8s")
		return nil
	case "destroy":
		return k8s.Destroy(ctx, o)
	case "manifests":
		m, err := k8s.Manifests(o)
		if err != nil {
			return err
		}
		fmt.Print(m)
		return nil
	case "tui":
		addr, stop, err := k8s.PortForward(ctx, o)
		if err != nil {
			return err
		}
		defer stop()
		log.SetOutput(io.Discard)
		return runTUI(tui.New(api.NewClient(addr), tui.Options{}), nil)
	}
	return fmt.Errorf("unknown k8s command %q\n\n%s", sub, usage)
}

// runTUI runs the TUI and, if given, the collector alongside it. The collector
// stops when the TUI quits; if it stops first (bad config, port in use,
// SIGTERM), it takes the TUI down with it.
func runTUI(m tea.Model, col *otelcol.Collector) error {
	p := tea.NewProgram(m, tea.WithColorProfile(colorProfile()))

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
		// Open the TUI only once the collector runs, so startup errors (port in
		// use, no eBPF permissions) print to a normal terminal.
		for col.GetState() != otelcol.StateRunning {
			select {
			case err := <-colErr:
				return fmt.Errorf("collector: %w", cmp.Or(err, errors.New("stopped during startup")))
			case <-time.After(10 * time.Millisecond):
			}
		}
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

// profilerConfig is the eBPF profiler setup shared by --profile and the agent.
var profilerConfig = []string{
	fmt.Sprintf("yaml:receivers::profiling::samples_per_second: %d", store.SamplesPerSecond),
	// Off-CPU time, so the LOOP view sees callbacks blocking in sleep or I/O.
	fmt.Sprintf("yaml:extensions::offcpu::threshold: %g", store.OffCPUThreshold),
	"yaml:receivers::profiling::probes: [offcpu]",
	"yaml:service::extensions: [offcpu]",
}

// localConfig is the collector config of kenbun itself (TUI and serve).
func localConfig(otlp, dump, logPath string, profile bool) ([]string, error) {
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
	if profile {
		uris = append(uris, profilerConfig...)
		uris = append(uris, "yaml:service::pipelines::profiles::receivers: [otlp, profiling]")
	}
	if logPath != "" {
		q := "[" + strconv.Quote(logPath) + "]"
		uris = append(uris,
			"yaml:service::telemetry::logs::output_paths: "+q,
			"yaml:service::telemetry::logs::error_output_paths: "+q,
		)
	}
	return uris, nil
}

// newCollector registers every component kenbun uses; uris pick the pipeline.
func newCollector(st *store.Store, uris []string) (*otelcol.Collector, error) {
	if err := featuregate.GlobalRegistry().Set("service.profilesSupport", true); err != nil {
		return nil, err
	}

	return otelcol.NewCollector(otelcol.CollectorSettings{
		BuildInfo: component.BuildInfo{
			Command:     "kenbun",
			Description: "OTLP profiles in your terminal",
			Version:     version,
		},
		Factories: func() (otelcol.Factories, error) {
			// The profiling receiver builds everywhere but only starts on Linux.
			receivers, err := otelcol.MakeFactoryMap(otlpreceiver.NewFactory(), profiler.NewFactory())
			if err != nil {
				return otelcol.Factories{}, err
			}
			exporters, err := otelcol.MakeFactoryMap(ingest.NewFactory(st), fileexporter.NewFactory(), otlpexporter.NewFactory())
			if err != nil {
				return otelcol.Factories{}, err
			}
			processors, err := otelcol.MakeFactoryMap(k8sattributesprocessor.NewFactory())
			if err != nil {
				return otelcol.Factories{}, err
			}
			extensions, err := extensionFactories()
			if err != nil {
				return otelcol.Factories{}, err
			}
			return otelcol.Factories{
				Receivers:  receivers,
				Exporters:  exporters,
				Processors: processors,
				Extensions: extensions,
				Telemetry:  otelconftelemetry.NewFactory(),
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

// colorProfile trusts truecolor whenever detection only finds 256 colors: that
// is what tmux (TERM=tmux-256color) and sudo (which drops COLORTERM) look like,
// on terminals that do support it. Bubble Tea would upgrade later from a
// terminal reply, but in tmux that reply is only read on the next keypress,
// so the first frames came out in the theme's 256-color approximations.
// Terminal.app is the one common terminal without truecolor.
func colorProfile() colorprofile.Profile {
	p := colorprofile.Detect(os.Stdout, os.Environ())
	if p == colorprofile.ANSI256 && os.Getenv("TERM_PROGRAM") != "Apple_Terminal" {
		return colorprofile.TrueColor
	}
	return p
}

// freeAddr returns addr if it can be listened on, otherwise a free port on the
// same host, so a local kenbun starts even when another collector holds 4317.
func freeAddr(addr string) string {
	ln, err := net.Listen("tcp", addr)
	if err == nil {
		ln.Close()
		return addr
	}
	host, _, _ := net.SplitHostPort(addr)
	if ln, err = net.Listen("tcp", net.JoinHostPort(host, "0")); err != nil {
		return addr // let the collector report the original error
	}
	defer ln.Close()
	return ln.Addr().String()
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
