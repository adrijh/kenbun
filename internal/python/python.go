// Package python profiles Python processes for machines without eBPF (macOS).
// The process list comes from the OS, by CPU; a process is only sampled while
// the TUI has it open, by what its own environment offers: Tachyon through its
// interpreter (3.15+), or py-spy from its venv or PATH.
package python

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/adrijh/kenbun/internal/store"
)

const (
	poll  = 2 * time.Second  // how often the process list refreshes, the TUI's pace
	chunk = 2 * time.Second  // one py-spy run: samples show up this often
	idle  = 10 * time.Second // a sampler nobody asked for in this long stops
)

// Source is a store whose process list is every Python process on the machine,
// and whose Stacks start py-spy on the process asked for.
type Source struct {
	*store.Store
	ctx       context.Context
	cancel    context.CancelFunc
	cache     string         // where py-spy gets installed, in the invoker's cache dir; "" if unknown
	wg        sync.WaitGroup // the samplers
	retention time.Duration

	mu       sync.Mutex
	snaps    []snapshot         // oldest first, one per poll
	samplers map[int64]*sampler // by PID
}

// snapshot is the Python processes running at t.
type snapshot struct {
	t     time.Time
	procs map[int64]proc
}

type proc struct {
	name string
	cpu  time.Duration // used since it started
}

type sampler struct {
	wanted time.Time // the last time the TUI asked for its stacks
	err    error     // why py-spy stopped, kept until the sampler is reaped
	stop   context.CancelFunc
}

// New starts polling processes until ctx is done or Close.
func New(ctx context.Context, st *store.Store, retention time.Duration) (*Source, error) {
	ctx, cancel := context.WithCancel(ctx)
	s := &Source{Store: st, ctx: ctx, cancel: cancel, retention: retention, samplers: map[int64]*sampler{}}
	if u, err := invoker(); err == nil {
		s.cache = filepath.Join(cacheDir(u), "py-spy")
	}
	if err := s.poll(); err != nil {
		cancel()
		return nil, err
	}
	go s.run()
	return s, nil
}

// Close stops the samplers and waits for them to clean up. Not the store.
func (s *Source) Close() {
	s.cancel()
	s.wg.Wait()
}

func (s *Source) run() {
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return // samplers' contexts derive from s.ctx: they stop too
		case <-t.C:
			if err := s.poll(); err != nil {
				log.Printf("python: %v", err)
			}
		}
	}
}

// poll takes a snapshot, drops the ones past retention and stops samplers
// nobody looks at or whose process exited.
func (s *Source) poll() error {
	procs, err := list()
	if err != nil {
		return fmt.Errorf("listing processes: %w", err)
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snaps = append(s.snaps, snapshot{now, procs})
	if i := slices.IndexFunc(s.snaps, func(sn snapshot) bool { return now.Sub(sn.t) < s.retention }); i > 0 {
		s.snaps = s.snaps[i:]
	}
	for pid, sm := range s.samplers {
		if _, ok := procs[pid]; !ok || now.Sub(sm.wanted) > idle {
			sm.stop()
			delete(s.samplers, pid)
		}
	}
	return nil
}

// Processes is the Python processes in [from, to) by the CPU they used then,
// from the OS's counters: the ones not sampled are listed too.
func (s *Source) Processes(_ context.Context, from, to time.Time) ([]store.ProcStat, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return processes(s.snaps, from, to), nil
}

func processes(snaps []snapshot, from, to time.Time) []store.ProcStat {
	// The counters at the window's ends: the last snapshot before each, or the
	// first one when the window starts before kenbun did.
	at := func(t time.Time) int {
		i, _ := slices.BinarySearchFunc(snaps, t, func(sn snapshot, t time.Time) int { return sn.t.Compare(t) })
		return i - 1
	}
	a, b := max(0, at(from)), at(to)
	if b < 0 {
		return nil
	}
	var out []store.ProcStat
	idx := map[string]int{}
	for pid, p := range snaps[b].procs {
		used := p.cpu - snaps[a].procs[pid].cpu // a process started since a used all of it since
		w := int64(math.Round(used.Seconds() * store.SamplesPerSecond))
		i, ok := idx[p.name]
		if !ok {
			i, idx[p.name] = len(out), len(out)
			out = append(out, store.ProcStat{Name: p.name})
		}
		out[i].Weight += w
		out[i].PIDs = append(out[i].PIDs, store.PIDStat{PID: pid, Weight: w})
	}
	store.SortProcesses(out)
	return out
}

// Oldest is when polling started, if earlier than the oldest sample: CPU
// averages over a window kenbun only saw part of divide by that part.
func (s *Source) Oldest(ctx context.Context) (time.Time, error) {
	t, err := s.Store.Oldest(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.snaps) > 0 && (t.IsZero() || s.snaps[0].t.Before(t)) {
		t = s.snaps[0].t
	}
	return t, err
}

// Stacks samples the process (all its PIDs, or only pid) from now on, and
// returns what was sampled so far.
func (s *Source) Stacks(ctx context.Context, process string, pid int64, from, to time.Time) ([]store.StackWeight, error) {
	if err := s.want(process, pid); err != nil {
		return nil, err
	}
	return s.Store.Stacks(ctx, process, pid, from, to)
}

// want starts or keeps alive a sampler on each PID asked for, and returns why
// py-spy stopped on them, if it did.
func (s *Source) want(process string, pid int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.snaps) == 0 {
		return nil
	}
	now := time.Now()
	var errs []error
	for p, pr := range s.snaps[len(s.snaps)-1].procs {
		if pr.name != process || (pid != 0 && p != pid) {
			continue
		}
		sm := s.samplers[p]
		if sm == nil {
			ctx, stop := context.WithCancel(s.ctx)
			sm = &sampler{stop: stop}
			s.samplers[p] = sm
			s.wg.Go(func() {
				if err := s.sample(ctx, process, p); err != nil {
					s.mu.Lock()
					sm.err = err
					s.mu.Unlock()
				}
			})
		}
		sm.wanted = now
		if sm.err != nil {
			errs = append(errs, sm.err)
		}
	}
	return errors.Join(errs...)
}

// sample runs a profiler on pid in chunks, storing each as it ends, until ctx
// is done or the profiler fails.
func (s *Source) sample(ctx context.Context, process string, pid int64) error {
	b, err := s.resolve(pid)
	if err != nil {
		return err
	}
	log.Printf("python: sampling pid %d (%s) with %s", pid, process, b.name)
	f, err := os.CreateTemp("", "kenbun-python-*.txt")
	if err != nil {
		return err
	}
	f.Close()
	defer os.Remove(f.Name())

	for ctx.Err() == nil {
		start := time.Now()
		args := b.args(pid, f.Name())
		out, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
		if ctx.Err() != nil || syscall.Kill(int(pid), 0) == syscall.ESRCH {
			return nil // stopped, or the process exited: not the profiler's fault
		}
		if err != nil {
			return fmt.Errorf("%s on pid %d: %s", b.name, pid, cmp.Or(lastLine(out), err.Error()))
		}
		raw, err := os.ReadFile(f.Name())
		if err != nil {
			return err
		}
		// All of a chunk's samples at its middle: profilers only give counts.
		ts := start.Add(time.Since(start) / 2)
		if err := s.Store.Ingest(ctx, parse(string(raw), process, pid, ts, b.frame)); err != nil && ctx.Err() == nil {
			return err
		}
	}
	return nil
}

// backend is a profiler: the command that samples pid for one chunk into
// out as folded stacks, and how to read their frames.
type backend struct {
	name  string
	args  func(pid int64, out string) []string
	frame func(string) (name, loc string)
}

var (
	rate = strconv.Itoa(store.SamplesPerSecond)
	secs = strconv.Itoa(int(chunk.Seconds()))
)

// tachyon is Python 3.15's profiling.sampling, run by the target's own
// interpreter: the profiler and its target must be the same version. On-CPU
// samples of every thread, like the eBPF profiler and py-spy record.
func tachyon(python string) backend {
	return backend{"tachyon", func(pid int64, out string) []string {
		return []string{python, "-I", "-B", "-m", "profiling.sampling", "attach", "-a", "--mode", "cpu",
			"-r", rate, "-d", secs, "--collapsed", "-o", out, strconv.FormatInt(pid, 10)}
	}, tachyonFrame}
}

func pySpy(bin string) backend {
	return backend{"py-spy", func(pid int64, out string) []string {
		return []string{bin, "record", "--pid", strconv.FormatInt(pid, 10), "--rate", rate, "--duration", secs,
			"--format", "raw", "--output", out}
	}, pySpyFrame}
}

// target is how a process runs, which says how to profile it.
type target struct {
	pid       int64
	uid       int
	exe       string // the interpreter, symlinks resolved on Linux
	args, env []string
}

// resolve picks the profiler for pid from how it runs: Tachyon if its
// interpreter has it (3.15+), else py-spy from its venv, its PATH, ours, or
// kenbun's cache.
func (s *Source) resolve(pid int64) (backend, error) {
	t, err := inspect(pid)
	if err != nil {
		return backend{}, fmt.Errorf("pid %d: %w", pid, err)
	}
	// -B: no root-owned __pycache__ in the user's Python.
	if trusted(t.exe) == nil && exec.Command(t.exe, "-I", "-B", "-c", "import profiling.sampling").Run() == nil {
		return tachyon(t.exe), nil
	}
	bin, err := s.find("py-spy", t)
	if err != nil {
		return backend{}, err
	}
	if bin == "" {
		return backend{}, &noProfiler{s, t}
	}
	return pySpy(bin), nil
}

// find looks for a tool in t's venv, t's PATH, ours and kenbun's cache: the
// first one there that kenbun may run as root, "" if none is anywhere.
func (s *Source) find(name string, t target) (string, error) {
	var dirs []string
	if len(t.args) > 0 && filepath.IsAbs(t.args[0]) {
		dirs = append(dirs, filepath.Dir(t.args[0])) // its venv's bin, even when not activated
	}
	for _, e := range t.env {
		if p, ok := strings.CutPrefix(e, "PATH="); ok {
			dirs = append(dirs, filepath.SplitList(p)...)
		}
	}
	dirs = append(dirs, filepath.SplitList(os.Getenv("PATH"))...)
	if s.cache != "" {
		dirs = append(dirs, filepath.Join(s.cache, "bin"))
	}
	var untrusted error
	for _, d := range dirs {
		bin := filepath.Join(d, name)
		if _, err := os.Stat(bin); err != nil {
			continue
		}
		if err := trusted(bin); err != nil {
			untrusted = cmp.Or(untrusted, err)
			continue
		}
		return bin, nil
	}
	return "", untrusted
}

// noProfiler: a process's Python has no Tachyon and py-spy is nowhere. The TUI
// offers to install py-spy (tui.Fixer).
type noProfiler struct {
	s *Source
	t target
}

func (e *noProfiler) Error() string {
	return fmt.Sprintf("no profiler for pid %d: its Python has no profiling.sampling (3.15+), and py-spy isn't in its venv or PATH", e.t.pid)
}

func (e *noProfiler) Fix() (string, func(context.Context) error) {
	return "install py-spy", func(ctx context.Context) error { return e.s.install(ctx, e.t) }
}

// install puts py-spy in kenbun's cache, for every process from then on, and
// retries the ones it was missing for. It runs as the user who ran kenbun,
// with t's pip or uv: their index and proxy settings apply, the files are
// theirs, and their projects don't change.
func (s *Source) install(ctx context.Context, t target) error {
	u, err := invoker()
	if err != nil {
		return err
	}
	if s.cache == "" {
		return errors.New("no cache directory to install py-spy in")
	}
	env := os.Environ()
	if strconv.Itoa(t.uid) == u.Uid {
		env = t.env // their own process: their index, proxy and credentials
	}
	env = append(env, "HOME="+u.HomeDir, "USER="+u.Username, "LOGNAME="+u.Username)
	run := func(args ...string) *exec.Cmd {
		c := exec.CommandContext(ctx, args[0], args[1:]...)
		c.Env, c.Dir = env, u.HomeDir
		asUser(c, u)
		return c
	}

	var args []string
	pythons := []string{t.exe}
	if len(t.args) > 0 && filepath.IsAbs(t.args[0]) {
		pythons = []string{t.args[0], t.exe} // its venv's, whose pip the user set up
	}
	for _, py := range pythons {
		if trusted(py) == nil && run(py, "-I", "-m", "pip", "--version").Run() == nil {
			args = []string{py, "-I", "-m", "pip", "install", "--disable-pip-version-check", "--upgrade", "--target", s.cache, "py-spy"}
			break
		}
	}
	if args == nil { // uv venvs come without pip
		uv, err := s.find("uv", t)
		if uv == "" {
			return cmp.Or(err, errors.New("no pip or uv to install py-spy with: pip install py-spy, or brew install py-spy"))
		}
		args = []string{uv, "pip", "install", "--python", t.exe, "--target", s.cache, "py-spy"}
	}
	log.Printf("python: installing py-spy: %s", strings.Join(args, " "))
	if out, err := run(args...).CombinedOutput(); err != nil {
		return fmt.Errorf("installing py-spy: %s", cmp.Or(lastLine(out), err.Error()))
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for pid, sm := range s.samplers {
		if sm.err != nil {
			sm.stop()
			delete(s.samplers, pid)
		}
	}
	return nil
}

// invoker is who ran kenbun: the user behind sudo, else whoever runs it.
func invoker() (*user.User, error) {
	if name := os.Getenv("SUDO_USER"); name != "" && os.Geteuid() == 0 {
		return user.Lookup(name)
	}
	return user.Current()
}

// cacheDir is kenbun's directory in u's cache.
func cacheDir(u *user.User) string {
	if runtime.GOOS == "darwin" {
		return filepath.Join(u.HomeDir, "Library", "Caches", "kenbun")
	}
	if d := os.Getenv("XDG_CACHE_HOME"); d != "" { // sudo drops it unless -E: then it's theirs
		return filepath.Join(d, "kenbun")
	}
	return filepath.Join(u.HomeDir, ".cache", "kenbun")
}

// asUser makes c run as u when kenbun is root.
func asUser(c *exec.Cmd, u *user.User) {
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	if os.Geteuid() == 0 && uid != 0 {
		c.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}}
	}
}

// trusted: kenbun runs as root, so it only runs executables that root or the
// user who ran sudo owns, and nobody else can write.
func trusted(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s: no owner", path)
	}
	owner := st.Uid == 0 || int(st.Uid) == os.Geteuid() || strconv.Itoa(int(st.Uid)) == os.Getenv("SUDO_UID")
	if !owner || fi.Mode()&0o022 != 0 {
		return fmt.Errorf("not running %s: someone else can change it", path)
	}
	return nil
}

// parse reads folded stacks, one per line, root first, with a count:
// "a;b;c 13". Tachyon's start with the thread, "tid:42".
func parse(raw, process string, pid int64, ts time.Time, frame func(string) (name, loc string)) []store.Sample {
	var out []store.Sample
	for line := range strings.Lines(raw) {
		line = strings.TrimSpace(line)
		sp := strings.LastIndexByte(line, ' ')
		if sp < 0 {
			continue
		}
		n, err := strconv.ParseInt(line[sp+1:], 10, 64)
		if err != nil {
			continue
		}
		parts := strings.Split(line[:sp], ";")
		thread := ""
		if t, ok := strings.CutPrefix(parts[0], "tid:"); ok {
			thread, parts = t, parts[1:]
		}
		frames, locs := make([]string, len(parts)), make([]string, len(parts))
		for i, p := range parts {
			j := len(parts) - 1 - i // leaf first
			frames[j], locs[j] = frame(p)
		}
		out = append(out, store.Sample{TS: ts, Process: process, PID: pid, Thread: thread, Frames: frames, Locs: locs, Weight: n})
	}
	return out
}

// pySpyFrame: "crunch (app.py:7)".
func pySpyFrame(f string) (name, loc string) {
	if k := strings.LastIndex(f, " ("); k >= 0 && strings.HasSuffix(f, ")") {
		return f[:k], f[k+2 : len(f)-1]
	}
	return f, ""
}

// tachyonFrame: "app.py:Work.crunch:7", with the qualified name.
func tachyonFrame(f string) (name, loc string) {
	i, j := strings.IndexByte(f, ':'), strings.LastIndexByte(f, ':')
	if i < 0 || j <= i {
		return f, ""
	}
	return f[i+1 : j], f[:i] + f[j:]
}

func lastLine(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

var pythonExe = regexp.MustCompile(`(?i)^python[0-9.]*[tw]?$`)

// isPython: path is a Python interpreter (python3.12, python3.13t, Python.app's Python).
func isPython(path string) bool { return pythonExe.MatchString(filepath.Base(path)) }

// name is what a Python process runs, its row in the process list: the script
// or -m module, or the title it gave itself (setproctitle: gunicorn, celery).
func name(args []string) string {
	if len(args) == 0 {
		return "python"
	}
	if !isPython(args[0]) {
		if f := strings.Fields(args[0]); len(f) > 0 {
			return strings.TrimSuffix(filepath.Base(f[0]), ":")
		}
	}
	for i := 1; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-m" && i+1 < len(args):
			return args[i+1]
		case a == "-c":
			return "python -c"
		case a == "-X" || a == "-W": // take a value
			i++
		case strings.HasPrefix(a, "-"):
		default:
			return filepath.Base(a)
		}
	}
	return "python"
}
