//go:build !linux

package python

import (
	"reflect"
	"testing"
	"time"
)

func TestCPUTime(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"0:01.50":    1500 * time.Millisecond,
		"12:34.56":   12*time.Minute + 34560*time.Millisecond,
		"1:02:03.00": time.Hour + 2*time.Minute + 3*time.Second,
		"2-01:00:00": 49 * time.Hour,
	} {
		if got := cpuTime(in); got != want {
			t.Errorf("cpuTime(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestProcArgs(t *testing.T) {
	b := []byte{2, 0, 0, 0}
	b = append(b, "/app/.venv/bin/python3\x00\x00\x00\x00python3\x00app.py\x00PATH=/app/.venv/bin:/usr/bin\x00HOME=/Users/a\x00\x00ptr_munge=\x00"...)
	exe, args, env := procArgs(b)
	if exe != "/app/.venv/bin/python3" || !reflect.DeepEqual(args, []string{"python3", "app.py"}) ||
		!reflect.DeepEqual(env, []string{"PATH=/app/.venv/bin:/usr/bin", "HOME=/Users/a"}) {
		t.Errorf("procArgs = %q, %q, %q", exe, args, env)
	}
}
