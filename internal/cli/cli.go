// Package cli answers one question per command (top, functions, loop, diff)
// in a few lines of text, or JSON: for scripts and AI agents, which can't
// drive the TUI and shouldn't read raw stacks. The analysis is the TUI's own
// (store.Top, store.Loop), so both always agree.
package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/adrijh/kenbun/internal/store"
)

// Source is what the commands read: *store.Store or *api.Client.
type Source interface {
	Processes(ctx context.Context, from, to time.Time) ([]store.ProcStat, error)
	Stacks(ctx context.Context, process string, pid int64, from, to time.Time) ([]store.StackWeight, error)
	ThreadStacks(ctx context.Context, process string, pid int64, from, to time.Time) ([]store.ThreadStack, error)
	Oldest(ctx context.Context) (time.Time, error)
}

// Query is what every command shares.
type Query struct {
	Src   Source
	Since time.Duration // window: [End-Since, End)
	End   time.Time     // zero: now
	PID   int64         // one process instance, not all with the name; 0: all
	Limit int           // rows; <= 0: all
	JSON  bool
	Out   io.Writer

	short bool // Since was cut to the data there is
}

// fit shortens the window to the data there is (kenbun just started, or a
// short capture), so CPU isn't averaged over time nothing was recorded in. A
// diff needs room for two windows, so it splits what there is in halves.
func (q *Query) fit(ctx context.Context, windows int) {
	if q.End.IsZero() {
		q.End = time.Now() // one now for fitting and querying
	}
	oldest, err := q.Src.Oldest(ctx)
	if err != nil || oldest.IsZero() { // unknown (an older kenbun): keep --since
		return
	}
	from, to := q.window()
	if !oldest.After(to.Add(-time.Duration(windows) * q.Since)) {
		return
	}
	have := to.Sub(oldest)
	if windows == 1 {
		q.Since = have.Truncate(time.Second) + time.Second // up: the window must reach the oldest sample
	} else if q.Since = (have / time.Duration(windows)).Truncate(time.Second); q.Since < time.Second {
		q.Since = time.Second
	}
	q.short = q.Since < to.Sub(from)
}

// last is the header's window: "last 5m", or "last 51s (all there is)".
func (q Query) last() string {
	if q.short {
		return "last " + since(q.Since) + " (all there is)"
	}
	return "last " + since(q.Since)
}

// name is the process as the headers show it: with its pid if one was picked.
func (q Query) name(proc string) string {
	if q.PID != 0 {
		return fmt.Sprintf("%s pid %d", proc, q.PID)
	}
	return proc
}

func (q Query) window() (from, to time.Time) {
	to = q.End
	if to.IsZero() {
		to = time.Now()
	}
	return to.Add(-q.Since), to
}

func (q Query) millicores(samples int64) int64 { return store.Millicores(samples, q.Since) }

type window struct {
	Since   string    `json:"since"`
	From    time.Time `json:"from"`
	To      time.Time `json:"to"`
	AllData bool      `json:"all_data,omitempty"` // since was cut to the data there is
}

func (q Query) head() window {
	from, to := q.window()
	return window{since(q.Since), from.UTC(), to.UTC(), q.short}
}

// Top: processes by CPU.
func Top(ctx context.Context, q Query) error {
	q.fit(ctx, 1)
	from, to := q.window()
	procs, err := q.Src.Processes(ctx, from, to)
	if err != nil {
		return err
	}
	var all int64
	for _, p := range procs {
		all += p.Weight
	}
	type pid struct {
		PID        int64 `json:"pid"`
		Millicores int64 `json:"millicores"`
	}
	type row struct {
		Process    string  `json:"process"`
		Millicores int64   `json:"millicores"`
		Samples    int64   `json:"samples"`
		Share      float64 `json:"share"`          // % of all CPU kenbun saw
		PIDs       []pid   `json:"pids,omitempty"` // when there are several, for --pid
	}
	rows := make([]row, 0, len(procs))
	for _, p := range limit(procs, q.Limit) {
		r := row{p.Name, q.millicores(p.Weight), p.Weight, pct(p.Weight, all), nil}
		for _, ps := range p.PIDs {
			r.PIDs = append(r.PIDs, pid{ps.PID, q.millicores(ps.Weight)})
		}
		if len(r.PIDs) < 2 {
			r.PIDs = nil
		}
		rows = append(rows, r)
	}
	if q.JSON {
		return q.json(struct {
			window
			Millicores int64 `json:"millicores"`
			Samples    int64 `json:"samples"`
			Processes  []row `json:"processes"`
		}{q.head(), q.millicores(all), all, rows})
	}
	fmt.Fprintf(q.Out, "all processes · %s · %dm CPU · %d samples\n\n", q.last(), q.millicores(all), all)
	if len(rows) == 0 {
		fmt.Fprintln(q.Out, "no samples")
		return nil
	}
	return table(q.Out, "CPU\tSHARE\tPIDS\t  PROCESS", rows, func(r row) string {
		pids := ""
		if len(r.PIDs) > 0 {
			pids = fmt.Sprint(len(r.PIDs))
		}
		return fmt.Sprintf("%dm\t%.1f%%\t%s\t  %s", r.Millicores, r.Share, pids, r.Process)
	})
}

// Functions: one process's hottest functions, self (the function itself) and
// total (with what it calls), as % of the process's CPU.
func Functions(ctx context.Context, q Query, proc string) error {
	q.fit(ctx, 1)
	from, to := q.window()
	stacks, err := q.Src.Stacks(ctx, proc, q.PID, from, to)
	if err != nil {
		return err
	}
	all := total(stacks)
	type row struct {
		Function string  `json:"function"`
		Location string  `json:"location,omitempty"` // file:line of most of its time
		Self     float64 `json:"self"`
		Total    float64 `json:"total"`
	}
	var rows []row
	for _, f := range store.Top(stacks, q.Limit) {
		rows = append(rows, row{f.Name, f.Loc, pct(f.Self, all), pct(f.Total, all)})
	}
	if q.JSON {
		return q.json(struct {
			window
			Process    string `json:"process"`
			PID        int64  `json:"pid,omitempty"`
			Millicores int64  `json:"millicores"`
			Samples    int64  `json:"samples"`
			Functions  []row  `json:"functions"`
		}{q.head(), proc, q.PID, q.millicores(all), all, rows})
	}
	fmt.Fprintf(q.Out, "%s · %s · %dm CPU · %d samples\n\n", q.name(proc), q.last(), q.millicores(all), all)
	if len(rows) == 0 {
		fmt.Fprintln(q.Out, "no samples for this process ('kenbun top' lists them)")
		return nil
	}
	return table(q.Out, "SELF\tTOTAL\t  FUNCTION", rows, func(r row) string {
		return fmt.Sprintf("%.1f%%\t%.1f%%\t  %s", r.Self, r.Total, at(r.Function, r.Location))
	})
}

// Diff: how one process's functions moved against the window before, in
// percentage points of its CPU, biggest change first.
func Diff(ctx context.Context, q Query, proc string) error {
	q.fit(ctx, 2)
	from, to := q.window()
	cur, err := q.Src.Stacks(ctx, proc, q.PID, from, to)
	if err != nil {
		return err
	}
	prev, err := q.Src.Stacks(ctx, proc, q.PID, from.Add(-q.Since), from)
	if err != nil {
		return err
	}
	curN, prevN := total(cur), total(prev)
	before := map[string]store.FuncStat{}
	for _, f := range store.Top(prev, 0) {
		before[f.Name] = f
	}
	type row struct {
		Function   string  `json:"function"`
		Location   string  `json:"location,omitempty"`
		Self       float64 `json:"self"`
		Total      float64 `json:"total"`
		DeltaSelf  float64 `json:"delta_self"`
		DeltaTotal float64 `json:"delta_total"`
	}
	var rows []row
	seen := map[string]bool{}
	add := func(name string, f store.FuncStat) {
		p := before[name]
		seen[name] = true
		self, tot := pct(f.Self, curN), pct(f.Total, curN)
		rows = append(rows, row{name, cmp.Or(f.Loc, p.Loc), self, tot, round(self - pct(p.Self, prevN)), round(tot - pct(p.Total, prevN))})
	}
	for _, f := range store.Top(cur, 0) {
		add(f.Name, f)
	}
	for name := range before { // gone since: now 0%
		if !seen[name] {
			add(name, store.FuncStat{})
		}
	}
	slices.SortFunc(rows, func(a, b row) int {
		return cmp.Or(cmp.Compare(math.Abs(b.DeltaSelf), math.Abs(a.DeltaSelf)),
			cmp.Compare(math.Abs(b.DeltaTotal), math.Abs(a.DeltaTotal)), cmp.Compare(a.Function, b.Function))
	})
	rows = limit(rows, q.Limit)
	if q.JSON {
		return q.json(struct {
			window
			Process          string `json:"process"`
			PID              int64  `json:"pid,omitempty"`
			Millicores       int64  `json:"millicores"`
			MillicoresBefore int64  `json:"millicores_before"`
			Samples          int64  `json:"samples"`
			SamplesBefore    int64  `json:"samples_before"`
			Functions        []row  `json:"functions"`
		}{q.head(), proc, q.PID, q.millicores(curN), q.millicores(prevN), curN, prevN, rows})
	}
	fmt.Fprintf(q.Out, "%s · %s vs the %s before · %dm → %dm CPU · %d → %d samples\n\n",
		q.name(proc), q.last(), since(q.Since), q.millicores(prevN), q.millicores(curN), prevN, curN)
	if curN == 0 || prevN == 0 {
		fmt.Fprintln(q.Out, "nothing to compare: no samples in one of the windows")
		return nil
	}
	return table(q.Out, "ΔSELF\tΔTOTAL\tSELF\tTOTAL\t  FUNCTION", rows, func(r row) string {
		return fmt.Sprintf("%+.1f\t%+.1f\t%.1f%%\t%.1f%%\t  %s", r.DeltaSelf, r.DeltaTotal, r.Self, r.Total, at(r.Function, r.Location))
	})
}

// Loop: each asyncio event loop of a process, how much of its time callbacks
// blocked it, and which ones, on CPU or waiting (sleep, blocking I/O).
func Loop(ctx context.Context, q Query, proc string) error {
	q.fit(ctx, 1)
	from, to := q.window()
	ts, err := q.Src.ThreadStacks(ctx, proc, q.PID, from, to)
	if err != nil {
		return err
	}
	type callback struct {
		Callback string  `json:"callback"`
		Location string  `json:"location,omitempty"` // file:line where it blocks most
		Blocked  float64 `json:"blocked"`            // % of loop time
		CPU      float64 `json:"cpu"`
		Wait     float64 `json:"wait"`
		Kind     string  `json:"kind"` // what dominates: cpu or wait
	}
	type loop struct {
		Thread    string     `json:"thread"`
		Blocked   float64    `json:"blocked"`
		CPU       float64    `json:"cpu"`
		Wait      float64    `json:"wait"`
		Idle      float64    `json:"idle"`
		Callbacks []callback `json:"callbacks"`
	}
	var loops []loop
	for _, l := range store.Loop(ts) {
		share := func(d time.Duration) float64 { return pct(int64(d), int64(l.Total)) }
		out := loop{l.Thread, share(l.Blocked()), share(l.CPU), share(l.Wait), share(l.Idle), []callback{}}
		for _, c := range limit(l.Callbacks, q.Limit) {
			kind := "cpu"
			if c.Wait > c.CPU {
				kind = "wait"
			}
			out.Callbacks = append(out.Callbacks, callback{c.Name, c.Loc, share(c.Blocked()), share(c.CPU), share(c.Wait), kind})
		}
		loops = append(loops, out)
	}
	if q.JSON {
		return q.json(struct {
			window
			Process string `json:"process"`
			PID     int64  `json:"pid,omitempty"`
			Loops   []loop `json:"loops"`
		}{q.head(), proc, q.PID, loops})
	}
	fmt.Fprintf(q.Out, "%s · %s\n", q.name(proc), q.last())
	if len(loops) == 0 {
		fmt.Fprintln(q.Out, "\nno asyncio event loop (needs Python frames; waiting needs off-CPU samples)")
		return nil
	}
	for _, l := range loops {
		fmt.Fprintf(q.Out, "\nthread %s: loop blocked %.1f%% (cpu %.1f%%, waiting %.1f%%), idle %.1f%%\n\n",
			l.Thread, l.Blocked, l.CPU, l.Wait, l.Idle)
		if err := table(q.Out, "BLOCKED\tCPU\tWAIT\tKIND\t  CALLBACK", l.Callbacks, func(c callback) string {
			return fmt.Sprintf("%.1f%%\t%.1f%%\t%.1f%%\t%s\t  %s", c.Blocked, c.CPU, c.Wait, c.Kind, at(c.Callback, c.Location))
		}); err != nil {
			return err
		}
	}
	return nil
}

// since formats a window like the flag that set it: 5m, not 5m0s.
func since(d time.Duration) string {
	s := d.Round(time.Second).String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// at is "name (file:line)", like py-spy, or just the name.
func at(name, loc string) string {
	if loc == "" {
		return name
	}
	return name + " (" + loc + ")"
}

func (q Query) json(v any) error {
	enc := json.NewEncoder(q.Out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func table[T any](w io.Writer, head string, rows []T, line func(T) string) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, head) // the last column (names) is left as is: unaligned
	for _, r := range rows {
		fmt.Fprintln(tw, line(r))
	}
	return tw.Flush()
}

func limit[T any](s []T, n int) []T {
	if n > 0 && len(s) > n {
		return s[:n]
	}
	return s
}

func total(stacks []store.StackWeight) int64 {
	var n int64
	for _, s := range stacks {
		n += s.Weight
	}
	return n
}

// pct is n as a percentage of of.
func pct(n, of int64) float64 {
	if of == 0 {
		return 0
	}
	return round(100 * float64(n) / float64(of))
}

func round(f float64) float64 { return math.Round(f*100) / 100 }
