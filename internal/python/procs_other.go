//go:build !linux

package python

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// list asks ps for the Python processes: macOS has no /proc. Twice, since
// the executable and the command line both may contain spaces.
func list() (map[int64]proc, error) {
	exes, err := ps("comm=")
	if err != nil {
		return nil, err
	}
	cmds, err := ps("time=,args=")
	if err != nil {
		return nil, err
	}
	out := map[int64]proc{}
	for pid, line := range cmds {
		f := strings.Fields(line)
		if len(f) < 2 || (!isPython(exes[pid]) && !isPython(f[1])) {
			continue
		}
		out[pid] = proc{name: name(f[1:]), cpu: cpuTime(f[0])}
	}
	return out, nil
}

// ps runs ps on every process and returns what it printed after each PID.
func ps(columns string) (map[int64]string, error) {
	b, err := exec.Command("ps", "-axo", "pid=,"+columns).Output()
	if err != nil {
		return nil, err
	}
	out := map[int64]string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(nil, 1<<20) // long command lines
	for sc.Scan() {
		pid, rest, _ := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		if n, err := strconv.ParseInt(pid, 10, 64); err == nil {
			out[n] = strings.TrimSpace(rest)
		}
	}
	return out, sc.Err()
}

// cpuTime parses ps's time column: [[dd-]hh:]mm:ss.cc.
func cpuTime(s string) time.Duration {
	var d time.Duration
	if days, rest, ok := strings.Cut(s, "-"); ok {
		n, _ := strconv.Atoi(days)
		d, s = time.Duration(n)*24*time.Hour, rest
	}
	var secs float64
	for p := range strings.SplitSeq(s, ":") {
		n, _ := strconv.ParseFloat(p, 64)
		secs = secs*60 + n
	}
	return d + time.Duration(math.Round(secs*1000))*time.Millisecond
}

// inspect is how pid runs: its interpreter, arguments, environment and owner.
func inspect(pid int64) (target, error) {
	t := target{pid: pid}
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", int(pid))
	if err != nil {
		return t, err
	}
	t.uid = int(kp.Eproc.Ucred.Uid)
	b, err := unix.SysctlRaw("kern.procargs2", int(pid))
	if err != nil {
		return t, err
	}
	t.exe, t.args, t.env = procArgs(b)
	return t, nil
}

// procArgs parses kern.procargs2: argc as an int32, the executable's path,
// NUL padding, then the arguments and the environment, each NUL-terminated.
func procArgs(b []byte) (exe string, args, env []string) {
	if len(b) < 4 {
		return "", nil, nil
	}
	argc := int(binary.LittleEndian.Uint32(b)) // Macs are little-endian
	f := strings.Split(string(b[4:]), "\x00")
	exe, i := f[0], 1
	for i < len(f) && f[i] == "" {
		i++
	}
	for ; i < len(f) && len(args) < argc; i++ {
		args = append(args, f[i])
	}
	for ; i < len(f) && f[i] != ""; i++ {
		env = append(env, f[i])
	}
	return exe, args, env
}
