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
	Stacks(ctx context.Context, process string, from, to time.Time) ([]store.StackWeight, error)
	ThreadStacks(ctx context.Context, process string, from, to time.Time) ([]store.ThreadStack, error)
}

// Query is what every command shares.
type Query struct {
	Src   Source
	Since time.Duration // window: [End-Since, End)
	End   time.Time     // zero: now
	Limit int           // rows; <= 0: all
	JSON  bool
	Out   io.Writer
}

func (q Query) window() (from, to time.Time) {
	to = q.End
	if to.IsZero() {
		to = time.Now()
	}
	return to.Add(-q.Since), to
}

// millicores turns on-CPU samples over the window into average CPU used, in
// thousandths of a core (Kubernetes' unit).
func (q Query) millicores(samples int64) int64 {
	return int64(math.Round(float64(samples) * 1000 / (store.SamplesPerSecond * q.Since.Seconds())))
}

type window struct {
	Since string    `json:"since"`
	From  time.Time `json:"from"`
	To    time.Time `json:"to"`
}

func (q Query) head() window {
	from, to := q.window()
	return window{since(q.Since), from.UTC(), to.UTC()}
}

// Top: processes by CPU.
func Top(ctx context.Context, q Query) error {
	from, to := q.window()
	procs, err := q.Src.Processes(ctx, from, to)
	if err != nil {
		return err
	}
	var all int64
	for _, p := range procs {
		all += p.Weight
	}
	type row struct {
		Process    string  `json:"process"`
		Millicores int64   `json:"millicores"`
		Samples    int64   `json:"samples"`
		Share      float64 `json:"share"` // % of all CPU kenbun saw
	}
	rows := make([]row, 0, len(procs))
	for _, p := range limit(procs, q.Limit) {
		rows = append(rows, row{p.Name, q.millicores(p.Weight), p.Weight, pct(p.Weight, all)})
	}
	if q.JSON {
		return q.json(struct {
			window
			Millicores int64 `json:"millicores"`
			Samples    int64 `json:"samples"`
			Processes  []row `json:"processes"`
		}{q.head(), q.millicores(all), all, rows})
	}
	fmt.Fprintf(q.Out, "all processes · last %s · %dm CPU · %d samples\n\n", since(q.Since), q.millicores(all), all)
	if len(rows) == 0 {
		fmt.Fprintln(q.Out, "no samples")
		return nil
	}
	return table(q.Out, "CPU\tSHARE\t  PROCESS", rows, func(r row) string {
		return fmt.Sprintf("%dm\t%.1f%%\t  %s", r.Millicores, r.Share, r.Process)
	})
}

// Functions: one process's hottest functions, self (the function itself) and
// total (with what it calls), as % of the process's CPU.
func Functions(ctx context.Context, q Query, proc string) error {
	from, to := q.window()
	stacks, err := q.Src.Stacks(ctx, proc, from, to)
	if err != nil {
		return err
	}
	all := total(stacks)
	type row struct {
		Function string  `json:"function"`
		Self     float64 `json:"self"`
		Total    float64 `json:"total"`
	}
	var rows []row
	for _, f := range store.Top(stacks, q.Limit) {
		rows = append(rows, row{f.Name, pct(f.Self, all), pct(f.Total, all)})
	}
	if q.JSON {
		return q.json(struct {
			window
			Process    string `json:"process"`
			Millicores int64  `json:"millicores"`
			Samples    int64  `json:"samples"`
			Functions  []row  `json:"functions"`
		}{q.head(), proc, q.millicores(all), all, rows})
	}
	fmt.Fprintf(q.Out, "%s · last %s · %dm CPU · %d samples\n\n", proc, since(q.Since), q.millicores(all), all)
	if len(rows) == 0 {
		fmt.Fprintln(q.Out, "no samples for this process ('kenbun top' lists them)")
		return nil
	}
	return table(q.Out, "SELF\tTOTAL\t  FUNCTION", rows, func(r row) string {
		return fmt.Sprintf("%.1f%%\t%.1f%%\t  %s", r.Self, r.Total, r.Function)
	})
}

// Diff: how one process's functions moved against the window before, in
// percentage points of its CPU, biggest change first.
func Diff(ctx context.Context, q Query, proc string) error {
	from, to := q.window()
	cur, err := q.Src.Stacks(ctx, proc, from, to)
	if err != nil {
		return err
	}
	prev, err := q.Src.Stacks(ctx, proc, from.Add(-q.Since), from)
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
		rows = append(rows, row{name, self, tot, round(self - pct(p.Self, prevN)), round(tot - pct(p.Total, prevN))})
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
			Millicores       int64  `json:"millicores"`
			MillicoresBefore int64  `json:"millicores_before"`
			Samples          int64  `json:"samples"`
			SamplesBefore    int64  `json:"samples_before"`
			Functions        []row  `json:"functions"`
		}{q.head(), proc, q.millicores(curN), q.millicores(prevN), curN, prevN, rows})
	}
	fmt.Fprintf(q.Out, "%s · last %s vs the %s before · %dm → %dm CPU · %d → %d samples\n\n",
		proc, since(q.Since), since(q.Since), q.millicores(prevN), q.millicores(curN), prevN, curN)
	if curN == 0 || prevN == 0 {
		fmt.Fprintln(q.Out, "nothing to compare: no samples in one of the windows")
		return nil
	}
	return table(q.Out, "ΔSELF\tΔTOTAL\tSELF\tTOTAL\t  FUNCTION", rows, func(r row) string {
		return fmt.Sprintf("%+.1f\t%+.1f\t%.1f%%\t%.1f%%\t  %s", r.DeltaSelf, r.DeltaTotal, r.Self, r.Total, r.Function)
	})
}

// Loop: each asyncio event loop of a process, how much of its time callbacks
// blocked it, and which ones, on CPU or waiting (sleep, blocking I/O).
func Loop(ctx context.Context, q Query, proc string) error {
	from, to := q.window()
	ts, err := q.Src.ThreadStacks(ctx, proc, from, to)
	if err != nil {
		return err
	}
	type callback struct {
		Callback string  `json:"callback"`
		Blocked  float64 `json:"blocked"` // % of loop time
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
			out.Callbacks = append(out.Callbacks, callback{c.Name, share(c.Blocked()), share(c.CPU), share(c.Wait), kind})
		}
		loops = append(loops, out)
	}
	if q.JSON {
		return q.json(struct {
			window
			Process string `json:"process"`
			Loops   []loop `json:"loops"`
		}{q.head(), proc, loops})
	}
	fmt.Fprintf(q.Out, "%s · last %s\n", proc, since(q.Since))
	if len(loops) == 0 {
		fmt.Fprintln(q.Out, "\nno asyncio event loop (needs Python frames; waiting needs off-CPU samples)")
		return nil
	}
	for _, l := range loops {
		fmt.Fprintf(q.Out, "\nthread %s: loop blocked %.1f%% (cpu %.1f%%, waiting %.1f%%), idle %.1f%%\n\n",
			l.Thread, l.Blocked, l.CPU, l.Wait, l.Idle)
		if err := table(q.Out, "BLOCKED\tCPU\tWAIT\tKIND\t  CALLBACK", l.Callbacks, func(c callback) string {
			return fmt.Sprintf("%.1f%%\t%.1f%%\t%.1f%%\t%s\t  %s", c.Blocked, c.CPU, c.Wait, c.Kind, c.Callback)
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
