package python

import (
	"bytes"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// clockTick is USER_HZ, the unit of /proc's CPU times: 100 on every Linux
// kenbun runs on (reading it needs cgo).
const clockTick = time.Second / 100

// list reads the Python processes from /proc.
func list() (map[int64]proc, error) {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	out := map[int64]proc{}
	for _, e := range ents {
		pid, err := strconv.ParseInt(e.Name(), 10, 64)
		if err != nil {
			continue
		}
		dir := "/proc/" + e.Name() + "/"
		cmdline, err := os.ReadFile(dir + "cmdline")
		if err != nil || len(cmdline) == 0 {
			continue // gone, or a kernel thread
		}
		args := nulls(cmdline)
		exe, _ := os.Readlink(dir + "exe") // also when argv[0] was renamed
		if !isPython(exe) && !isPython(args[0]) {
			continue
		}
		stat, err := os.ReadFile(dir + "stat")
		if err != nil {
			continue
		}
		out[pid] = proc{name: name(args), cpu: statCPU(string(stat))}
	}
	return out, nil
}

// statCPU is utime + stime from /proc/<pid>/stat, fields 14 and 15. They're
// counted after the command, which is in parentheses and may contain spaces.
func statCPU(stat string) time.Duration {
	f := strings.Fields(stat[strings.LastIndexByte(stat, ')')+1:])
	if len(f) < 13 {
		return 0
	}
	utime, _ := strconv.ParseInt(f[11], 10, 64)
	stime, _ := strconv.ParseInt(f[12], 10, 64)
	return time.Duration(utime+stime) * clockTick
}

// inspect is how pid runs: its interpreter, arguments, environment and owner.
func inspect(pid int64) (target, error) {
	t := target{pid: pid}
	dir := "/proc/" + strconv.FormatInt(pid, 10) + "/"
	fi, err := os.Stat(dir)
	if err != nil {
		return t, err
	}
	t.uid = int(fi.Sys().(*syscall.Stat_t).Uid)
	if t.exe, err = os.Readlink(dir + "exe"); err != nil {
		return t, err
	}
	cmdline, err := os.ReadFile(dir + "cmdline")
	if err != nil {
		return t, err
	}
	environ, err := os.ReadFile(dir + "environ")
	if err != nil {
		return t, err
	}
	t.args, t.env = nulls(cmdline), nulls(environ)
	return t, nil
}

// nulls splits /proc's NUL-terminated lists.
func nulls(b []byte) []string {
	return strings.Split(string(bytes.TrimRight(b, "\x00")), "\x00")
}
