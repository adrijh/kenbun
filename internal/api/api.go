// Package api serves the store over HTTP for remote TUIs (and later the web
// view). Times are unix nanoseconds, ranges are [from, to).
//
//	GET /api/processes?from=&to=          → []store.ProcStat
//	GET /api/stacks?process=&from=&to=    → []store.StackWeight
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
		ss, err := st.Stacks(r.Context(), r.URL.Query().Get("process"), from, to)
		reply(w, ss, err)
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

func (c *Client) Stacks(ctx context.Context, process string, from, to time.Time) ([]store.StackWeight, error) {
	q := rangeQuery(from, to)
	q.Set("process", process)
	var out []store.StackWeight
	return out, c.get(ctx, "/api/stacks", q, &out)
}

func rangeQuery(from, to time.Time) url.Values {
	return url.Values{
		"from": {strconv.FormatInt(from.UnixNano(), 10)},
		"to":   {strconv.FormatInt(to.UnixNano(), 10)},
	}
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
