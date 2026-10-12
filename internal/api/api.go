// Package api serves the store over HTTP for remote TUIs (and later the web
// view). Times are unix nanoseconds, ranges are [from, to).
//
//	GET /api/processes?from=&to=                  → []store.ProcStat
//	GET /api/stacks?process=&pid=&from=&to=       → []store.StackWeight
//	GET /api/threadstacks?process=&pid=&from=&to= → []store.ThreadStack
//	GET /api/oldest                               → {"oldest": unix ns, 0 if empty}
//
// pid is optional: without it (or 0), all of the process's PIDs.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/adrijh/kenbun/internal/store"
)

func Handler(st *store.Store) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/processes", func(w http.ResponseWriter, r *http.Request) {
		from, to, err := timeRange(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		ps, err := st.Processes(r.Context(), from, to)
		reply(w, ps, err)
	})
	mux.HandleFunc("GET /api/stacks", func(w http.ResponseWriter, r *http.Request) {
		from, to, err := timeRange(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		ss, err := st.Stacks(r.Context(), r.URL.Query().Get("process"), pid(r), from, to)
		reply(w, ss, err)
	})
	mux.HandleFunc("GET /api/threadstacks", func(w http.ResponseWriter, r *http.Request) {
		from, to, err := timeRange(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		ts, err := st.ThreadStacks(r.Context(), r.URL.Query().Get("process"), pid(r), from, to)
		reply(w, ts, err)
	})
	mux.HandleFunc("GET /api/oldest", func(w http.ResponseWriter, r *http.Request) {
		t, err := st.Oldest(r.Context())
		var ns int64
		if !t.IsZero() {
			ns = t.UnixNano()
		}
		reply(w, map[string]int64{"oldest": ns}, err)
	})
	return mux
}

func timeRange(r *http.Request) (from, to time.Time, err error) {
	q := r.URL.Query()
	f, err1 := strconv.ParseInt(q.Get("from"), 10, 64)
	t, err2 := strconv.ParseInt(q.Get("to"), 10, 64)
	if err1 != nil || err2 != nil {
		return from, to, fmt.Errorf("from and to must be unix nanoseconds")
	}
	return time.Unix(0, f), time.Unix(0, t), nil
}

func pid(r *http.Request) int64 {
	n, _ := strconv.ParseInt(r.URL.Query().Get("pid"), 10, 64)
	return n
}

func reply(w http.ResponseWriter, v any, err error) {
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// Client reads from a kenbun server. It has the same query methods as
// *store.Store, so the TUI works with either.
type Client struct {
	base string
	hc   *http.Client
}

// NewClient takes "host:port" or a full base URL.
func NewClient(addr string) *Client {
	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	return &Client{base: strings.TrimSuffix(addr, "/"), hc: &http.Client{Timeout: 10 * time.Second}}
}

func (c *Client) Processes(ctx context.Context, from, to time.Time) ([]store.ProcStat, error) {
	var out []store.ProcStat
	return out, c.get(ctx, "/api/processes", rangeQuery(from, to), &out)
}

func (c *Client) Stacks(ctx context.Context, process string, pid int64, from, to time.Time) ([]store.StackWeight, error) {
	q := procQuery(process, pid, from, to)
	var out []store.StackWeight
	return out, c.get(ctx, "/api/stacks", q, &out)
}

func (c *Client) ThreadStacks(ctx context.Context, process string, pid int64, from, to time.Time) ([]store.ThreadStack, error) {
	q := procQuery(process, pid, from, to)
	var out []store.ThreadStack
	return out, c.get(ctx, "/api/threadstacks", q, &out)
}

func (c *Client) Oldest(ctx context.Context) (time.Time, error) {
	var out struct{ Oldest int64 }
	if err := c.get(ctx, "/api/oldest", url.Values{}, &out); err != nil || out.Oldest == 0 {
		return time.Time{}, err
	}
	return time.Unix(0, out.Oldest), nil
}

func rangeQuery(from, to time.Time) url.Values {
	return url.Values{
		"from": {strconv.FormatInt(from.UnixNano(), 10)},
		"to":   {strconv.FormatInt(to.UnixNano(), 10)},
	}
}

func procQuery(process string, pid int64, from, to time.Time) url.Values {
	q := rangeQuery(from, to)
	q.Set("process", process)
	if pid != 0 {
		q.Set("pid", strconv.FormatInt(pid, 10))
	}
	return q
}

func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: %s: %s", path, resp.Status, strings.TrimSpace(string(msg)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
