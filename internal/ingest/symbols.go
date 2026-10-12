package ingest

import (
	"bufio"
	"bytes"
	"cmp"
	"debug/elf"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ianlancetaylor/demangle"
)

// Native frames arrive unnamed: the eBPF profiler sends their address in the
// binary (its own virtual addresses, return addresses already minus 1) and the
// binary's GNU build ID, and leaves naming to the backend. symbolize names them
// from ELF symbol tables on this machine, never the network: the debug file a
// distro's -debug package installs, else the binary's own symbols (exported
// ones only, once stripped). A file is only used if its build ID matches, so
// frames from elsewhere (OTLP senders, captures) stay unnamed, never misnamed.
// Any failure (no file, unreadable, malformed, not Linux) leaves a frame
// unnamed, as before.

const (
	debugDir   = "/usr/lib/debug/.build-id"
	maxStrtab  = 32 << 20         // larger symbol tables (a full Chromium) aren't loaded
	maxSyms    = 4 << 20          // nor more symbols than this
	evictAfter = 30 * time.Minute // tables unused this long are dropped, like samples
	retryAfter = time.Minute      // a binary not found (yet) is looked for again
)

var symbols = struct {
	sync.Mutex
	m     map[string]*symEntry // by build ID
	swept time.Time
}{m: map[string]*symEntry{}}

type symEntry struct {
	once   sync.Once
	done   atomic.Bool // tab and loaded are set
	tab    *symtab     // nil: no symbols for this build ID
	loaded time.Time
	used   atomic.Int64 // unix seconds
}

// symbolize returns the function containing addr in the binary with this build
// ID, named file (its base name) and mapped by process pid, or "" if there's
// none.
func symbolize(buildID, file string, pid int64, addr uint64) string {
	if runtime.GOOS != "linux" || buildID == "" {
		return ""
	}
	e := symEntryFor(buildID)
	e.once.Do(func() {
		e.tab, e.loaded = loadSymbols(buildID, file, pid), time.Now()
		e.done.Store(true)
	})
	e.used.Store(time.Now().Unix())
	if e.tab == nil {
		return ""
	}
	return e.tab.lookup(addr)
}

func symEntryFor(buildID string) *symEntry {
	symbols.Lock()
	defer symbols.Unlock()
	now := time.Now()
	if now.Sub(symbols.swept) > time.Minute {
		symbols.swept = now
		for id, e := range symbols.m {
			if now.Sub(time.Unix(e.used.Load(), 0)) > evictAfter {
				delete(symbols.m, id)
			}
		}
	}
	e := symbols.m[buildID]
	if e == nil || (e.done.Load() && e.tab == nil && now.Sub(e.loaded) > retryAfter) {
		e = &symEntry{}
		symbols.m[buildID] = e
	}
	return e
}

// libDirs are searched for a binary when its process is gone (a capture
// read later): the profiler only reports a binary's base name.
var libDirs = []string{
	"/usr/lib", "/usr/lib64", "/lib", "/lib64", "/usr/local/lib", "/usr/bin", "/usr/local/bin",
	"/usr/lib/x86_64-linux-gnu", "/lib/x86_64-linux-gnu", "/usr/lib/aarch64-linux-gnu", "/lib/aarch64-linux-gnu",
}

// loadSymbols reads the first file that has this build ID and function
// symbols: its debug file, the binary as the process maps it, or a binary of
// that name in the usual directories.
func loadSymbols(buildID, base string, pid int64) *symtab {
	if _, err := hex.DecodeString(buildID); err != nil || len(buildID) < 3 {
		return nil // not a path to build from
	}
	paths := []string{filepath.Join(debugDir, buildID[:2], buildID[2:]+".debug")}
	if base = filepath.Base(base); base != "." && base != "/" {
		paths = append(paths, mapped(pid, base)...)
		for _, d := range libDirs {
			paths = append(paths, filepath.Join(d, base))
		}
	}
	for _, p := range paths {
		if t := readSymbols(p, buildID); t != nil {
			return t
		}
	}
	return nil
}

// mapped returns where process pid has files named base mapped, opened through
// /proc so it works in containers and for files deleted since (upgrades).
func mapped(pid int64, base string) []string {
	if pid <= 0 {
		return nil
	}
	maps, err := os.ReadFile(fmt.Sprintf("/proc/%d/maps", pid))
	if err != nil {
		return nil
	}
	var out []string
	for line := range strings.Lines(string(maps)) {
		// 7f12-7f34 r-xp 00001000 fd:01 1234   /usr/lib/libc.so.6 (deleted)
		f := strings.Fields(line)
		if len(f) < 6 || !strings.HasPrefix(f[5], "/") {
			continue
		}
		p := strings.Join(f[5:], " ")
		deleted := strings.HasSuffix(p, " (deleted)")
		p = strings.TrimSuffix(p, " (deleted)")
		if filepath.Base(p) != base {
			continue
		}
		if deleted {
			out = append(out, fmt.Sprintf("/proc/%d/map_files/%s", pid, f[0]))
		} else {
			out = append(out, fmt.Sprintf("/proc/%d/root%s", pid, p))
		}
	}
	return slices.Compact(out)
}

type sym struct {
	addr, size uint64
	name       string
}

func readSymbols(path, buildID string) (t *symtab) {
	defer func() {
		if recover() != nil { // a malformed file must not take kenbun down
			t = nil
		}
	}()
	f, err := elf.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	if f.Class != elf.ELFCLASS64 || gnuBuildID(f) != buildID {
		return nil
	}
	var syms []sym
	for _, name := range []string{".symtab", ".dynsym"} {
		if s, err := funcSymbols(f, f.Section(name)); err == nil {
			syms = append(syms, s...)
		}
	}
	if len(syms) == 0 {
		return nil
	}
	return newSymtab(syms)
}

// funcSymbols reads a symbol section's functions without decoding every
// symbol into Go values first, as (*elf.File).Symbols does.
func funcSymbols(f *elf.File, sec *elf.Section) ([]sym, error) {
	if sec == nil || sec.Type == elf.SHT_NOBITS || int(sec.Link) >= len(f.Sections) {
		return nil, nil // absent, or emptied in a debug file
	}
	strs := f.Sections[sec.Link]
	if strs.Type == elf.SHT_NOBITS || strs.Size > maxStrtab || sec.Size/elf.Sym64Size > maxSyms {
		return nil, nil
	}
	str, err := strs.Data()
	if err != nil {
		return nil, err
	}
	var out []sym
	r := bufio.NewReader(sec.Open())
	var b [elf.Sym64Size]byte
	for {
		if _, err := io.ReadFull(r, b[:]); err != nil {
			if err == io.EOF {
				return out, nil
			}
			return out, err
		}
		nameOff := uint64(f.ByteOrder.Uint32(b[0:4]))
		value, size := f.ByteOrder.Uint64(b[8:16]), f.ByteOrder.Uint64(b[16:24])
		if elf.ST_TYPE(b[4]) != elf.STT_FUNC || value == 0 || size == 0 || nameOff >= uint64(len(str)) {
			continue
		}
		name := str[nameOff:]
		if i := bytes.IndexByte(name, 0); i >= 0 {
			name = name[:i]
		}
		if len(name) > 0 {
			out = append(out, sym{value, size, string(name)})
		}
	}
}

// gnuBuildID is the hex GNU build ID note of f, as the profiler reports it.
func gnuBuildID(f *elf.File) string {
	for _, s := range f.Sections {
		if s.Type != elf.SHT_NOTE {
			continue
		}
		d, err := s.Data()
		if err != nil {
			continue
		}
		for len(d) >= 12 {
			namesz, descsz, typ := f.ByteOrder.Uint32(d[0:4]), f.ByteOrder.Uint32(d[4:8]), f.ByteOrder.Uint32(d[8:12])
			name := 12 + (uint64(namesz)+3)&^3
			end := name + (uint64(descsz)+3)&^3
			if end > uint64(len(d)) {
				break
			}
			if typ == 3 && namesz == 4 && string(d[12:15]) == "GNU" { // NT_GNU_BUILD_ID
				return hex.EncodeToString(d[name : name+uint64(descsz)])
			}
			d = d[end:]
		}
	}
	return ""
}

// symtab holds functions sorted by address, demangling names as they're used.
type symtab struct {
	syms []sym

	mu        sync.Mutex
	demangled map[int]string
}

func newSymtab(syms []sym) *symtab {
	// .symtab comes first and wins ties: it has the internal names.
	slices.SortStableFunc(syms, func(a, b sym) int { return cmp.Compare(a.addr, b.addr) })
	syms = slices.CompactFunc(syms, func(a, b sym) bool { return a.addr == b.addr })
	return &symtab{syms: slices.Clip(syms), demangled: map[int]string{}}
}

// lookup names the function addr is inside of. Between functions (code with
// no symbol, as stripped binaries' internal functions) it's "": the nearest
// symbol below would be some other function.
func (t *symtab) lookup(addr uint64) string {
	i := sort.Search(len(t.syms), func(i int) bool { return t.syms[i].addr > addr }) - 1
	if i < 0 || addr >= t.syms[i].addr+t.syms[i].size {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	n, ok := t.demangled[i]
	if !ok {
		n = demangle.Filter(t.syms[i].name, demangle.NoParams, demangle.NoTemplateParams)
		t.demangled[i] = n
	}
	return n
}
