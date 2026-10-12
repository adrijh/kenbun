package ingest

import (
	"debug/elf"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// systemLibc is a stripped shared library with exported functions and a GNU
// build ID, on any Linux this runs on.
func systemLibc(t *testing.T) (path, buildID string, f *elf.File) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("symbolization is Linux only")
	}
	for _, p := range []string{"/usr/lib/libc.so.6", "/lib/x86_64-linux-gnu/libc.so.6", "/lib/aarch64-linux-gnu/libc.so.6", "/usr/lib64/libc.so.6"} {
		if f, err := elf.Open(p); err == nil {
			t.Cleanup(func() { f.Close() })
			if id := gnuBuildID(f); id != "" {
				return p, id, f
			}
		}
	}
	t.Skip("no libc with a build ID here")
	return
}

func TestSymbolize(t *testing.T) {
	path, id, f := systemLibc(t)
	dyn, err := f.DynamicSymbols()
	if err != nil {
		t.Fatal(err)
	}
	var fn elf.Symbol
	for _, s := range dyn {
		if s.Name == "getpid" && s.Size > 1 {
			fn = s
		}
	}
	if fn.Name == "" {
		t.Skip("libc exports no sized getpid")
	}
	isGetpid := func(got string) bool { // aliases share the address
		for _, s := range dyn {
			if s.Value == fn.Value && s.Name == got {
				return true
			}
		}
		return false
	}
	// An address inside getpid, and the base name, as the profiler reports
	// them. With no process, libc is found in the usual directories.
	if got := symbolize(id, filepath.Base(path), 0, fn.Value+1); !isGetpid(got) {
		t.Errorf("symbolize(getpid+1) = %q, want getpid or an alias", got)
	}

	// Through a live process's mappings: a fresh cache, a binary only there.
	sleep := exec.Command("sleep", "5")
	if err := sleep.Start(); err != nil {
		t.Skip(err)
	}
	defer sleep.Process.Kill()
	time.Sleep(100 * time.Millisecond) // exec'd and mapped libc
	libDirs := libDirs
	defer func() { symbolsReset(); setLibDirs(libDirs) }()
	symbolsReset()
	setLibDirs(nil)
	if got := symbolize(id, filepath.Base(path), int64(sleep.Process.Pid), fn.Value+1); !isGetpid(got) {
		t.Errorf("symbolize through /proc/<pid>/maps = %q, want getpid", got)
	}
}

func symbolsReset() {
	symbols.Lock()
	defer symbols.Unlock()
	clear(symbols.m)
}

func setLibDirs(d []string) { libDirs = d }

// Every way of not finding symbols leaves the frame unnamed, quietly.
func TestSymbolizeMissing(t *testing.T) {
	path, id, _ := systemLibc(t)
	junk := filepath.Join(t.TempDir(), "libjunk.so")
	os.WriteFile(junk, []byte("\x7fELF\x02\x01\x01 not really an elf file"), 0o644)
	for name, c := range map[string]struct {
		id, file string
		pid      int64
		addr     uint64
	}{
		"other build":    {"00112233445566778899aabbccddeeff00112233", path, 0, 0x1000},
		"no file":        {"1122334455667788990011223344556677889900", "/nonexistent/libx.so", 0, 0x1000},
		"malformed file": {"2233445566778899001122334455667788990011", junk, 0, 0x1000},
		"path in name":   {"3344556677889900112233445566778899001122", "../../etc/libc.so.6", 0, 0x1000},
		"gone process":   {"4455667788990011223344556677889900112233", "/nonexistent/liby.so", 1 << 30, 0x1000},
		"no build ID":    {"", path, 0, 0x1000},
		"not hex":        {"../../../etc/passwd", path, 0, 0x1000},
		"no function":    {id, path, 0, 0}, // before any code
	} {
		if got := symbolize(c.id, c.file, c.pid, c.addr); got != "" {
			t.Errorf("%s: symbolize = %q, want unnamed", name, got)
		}
	}
}

func TestSymtabLookup(t *testing.T) {
	tab := newSymtab([]sym{
		{0x2000, 0x10, "_ZN3foo3barEv"},
		{0x1000, 0x100, "plain"},
		{0x1000, 0x100, "alias"}, // same address, later: dropped
		{0x3000, 0x20, "_ZN4core3fmt5write17h0123456789abcdefE"},
	})
	for addr, want := range map[uint64]string{
		0x0fff: "", 0x1000: "plain", 0x10ff: "plain",
		0x1100: "", // past plain's end, before foo::bar: not plain
		0x2008: "foo::bar", 0x2010: "",
		0x3001: "core::fmt::write",
		0x9000: "",
	} {
		if got := tab.lookup(addr); got != want {
			t.Errorf("lookup(%#x) = %q, want %q", addr, got, want)
		}
	}
}
