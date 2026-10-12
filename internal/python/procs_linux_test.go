package python

import (
	"os/exec"
	"testing"
	"time"
)

func TestStatFields(t *testing.T) {
	// The command may hold spaces and parentheses; utime 250, stime 50 ticks.
	stat := "4242 (python3 (x) y) S 17 4242 4242 0 -1 4194560 1234 0 0 0 250 50 0 0 20 0 2 0 99 0"
	if ppid, cpu := statFields(stat); ppid != 17 || cpu != 3*time.Second {
		t.Errorf("statFields = %d, %v, want 17, 3s", ppid, cpu)
	}
}

// list finds this test's own Python child.
func TestList(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3")
	}
	cmd := exec.Command(py, "-c", "import time; time.sleep(30)")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()

	procs, err := list()
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := procs[int64(cmd.Process.Pid)]; !ok || p.name != "python -c" {
		t.Errorf("list()[%d] = %+v, %v", cmd.Process.Pid, p, ok)
	}
}
