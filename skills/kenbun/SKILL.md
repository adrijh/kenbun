---
name: kenbun
description: Investigate CPU usage and asyncio event-loop blocking with kenbun's query commands (kenbun top, functions, diff, loop; --k8s for a Kubernetes cluster, --file for a capture). Use this whenever someone asks what is eating CPU on a machine or cluster, why a service, pod or process is slow or got slower (after a deploy, under load), where a process spends its time, or whether a Python asyncio app blocks its event loop, and kenbun is installed or deployed, even if they don't name kenbun or say "profile".
---

# Investigating with kenbun

kenbun keeps a rolling 30 minutes of CPU profiles (from an eBPF profiler on every
node, or from OTLP senders) and answers questions about them in a few lines. Every
query is one command; read its output, then decide the next one. Don't open the
TUI (`kenbun` with no query): it is interactive and made for people.

## Where the data is

Every query takes one source flag; pick it once and keep it on every command.

- **A cluster:** `--k8s` (current kubeconfig context) or `--context <ctx>`
  port-forwards to the kenbun that `kenbun k8s deploy` installed. "port-forward"
  errors mean it isn't deployed there: ask before running `kenbun k8s deploy`,
  it installs a privileged DaemonSet.
- **This machine:** no flag. It asks the kenbun running here: the user's TUI
  (`kenbun`) or a headless `kenbun serve`. Profiling the machine needs root, so if
  nothing answers, ask the user to start `sudo kenbun --profile` (or
  `sudo kenbun serve --profile`) rather than running sudo yourself, and give it a
  minute to collect.
- **A capture:** `--file capture.jsonl[.gz]` (from `kenbun --dump`), offline;
  `--since` counts back from its end.
- **Another kenbun:** `--addr host:port`.

## The queries

| Question | Command |
|---|---|
| What uses the CPU? | `top` |
| Where does this process spend it? | `functions <process>` |
| What changed? (regressions, spikes) | `diff <process>` |
| Is this asyncio app blocking its loop? | `loop <process>` |

Shared flags: `--since 5m` (window; data goes back 30m, so `diff` takes at most
`--since 15m`, as it also reads the window before), `--limit 20`, `--json`.
Process names come from `top`, exactly as printed: `service/container` for pods
(`argo-cd/redis`), the executable elsewhere (`kubelet`, `python3.14`). kenbun hides
itself, so its own pods (the `kenbun` deployment and the `kenbun-profiler` agents,
around 10–20m CPU per node) never appear; look at them with `kubectl` if they matter.

Text output is compact and meant to be read. Use `--json` only to compute
something across many rows. The usual case: runtime and native frames crowd the
top of `functions`, and you want the program's own code. Take every row and drop
them and sort by total, which surfaces the program's call path (kernel frames
remain, recognizable by their names):

```bash
kenbun functions crio --k8s --since 15m --limit 0 --json |
  jq -r '.functions | sort_by(-.total)[] | select(.function | test("^(runtime|internal|syscall|sync)\\.|^\\[") | not)
         | "\(.self)\t\(.total)\t\(.function)"' | head -30
```

## A typical investigation

1. `top`: find the process. CPU is in millicores (`250m` = a quarter core, average
   over the window); share is of all CPU kenbun saw.
2. `functions <process>`: **self** is time in the function itself, **total**
   includes what it calls. High self is where CPU burns; high total with low self
   is a caller on the way there. Read down until names say something about the
   program, not the runtime. Python and Go functions come with the line most of
   their time is on, `fib (/app/src/main.py:18)`: open that file there. Paths are
   as seen on the profiled machine (inside the container), so map them onto the
   repo by their tail.
3. If it got worse: `diff <process>` with `--since` about how long ago it started.
   Deltas are percentage points of the process's CPU, biggest first; the header
   has CPU before → after.
4. If it is a Python asyncio service: `loop <process>`.

A header saying `last 2m (all there is)` means kenbun holds less than the
`--since` you asked for (it just started, or a short capture): numbers cover what
there is, and `diff` then compares its two halves. Before concluding, check the
sample counts in each header. kenbun samples each CPU
97 times a second, so a process at 5m CPU over 5 minutes has ~150 samples: shifts
of a few points are noise there. Prefer a longer `--since` for quiet processes, and
say how much data a claim rests on.

## Reading frames

- `[libpython3.14.so.1.0]`, `[libc.so.6]`: native code with no symbols, named after
  its library. Near 100% total usually just means "everything runs through the
  interpreter"; look for the named functions below it.
- Kernel functions (`arch_local_irq_enable`, `finish_task_switch`, syscalls) are
  time in the kernel on the process's behalf: I/O, scheduling, page faults.
- Go: `runtime.scanobject`, `runtime.gcBgMarkWorker` mean garbage collection;
  `runtime.mallocgc` allocation. High together: the process allocates too much,
  so find who allocates in `functions`.
- Python names are qualified (`JSONDecoder.decode`, `handler.<locals>.<genexpr>`).

## asyncio: `loop`

The event loop runs one callback at a time, so any callback that takes long
stalls every other request. `loop` reports, per loop thread:

- **blocked %**: time callbacks held the loop. **cpu** means computing;
  **wait** means the callback sat off-CPU without awaiting: `time.sleep`,
  `requests`, a sync DB driver, file I/O, a lock.
- **idle %**: the loop waiting for work. High idle with high latency points away
  from the loop (downstream calls, too few workers).
- **callbacks**: blocking time charged to the coroutine or callback that held the
  loop, normally the request handler, with the line in it where it blocks most:
  for **wait** that is usually the blocking call itself (`time.sleep(...)`).

What to recommend: for **wait**, await an async equivalent or move the call to
`asyncio.to_thread()` / an executor; for **cpu**, split the work, or move it to
a process pool if it's heavy. Waiting is estimated from a sample of context
switches, so it's noisier than cpu: trust it over a minute or more.

"no asyncio event loop" means no stack showed `BaseEventLoop._run_once`: not
asyncio, uvloop (compiled, no Python frames), or no samples in the window.

## Reporting

Lead with the answer: which process, which functions, how much CPU (millicores and
share), over what window and how many samples. Then the likely cause in code
terms and the next step. Quote the few rows that support it, not the whole table.
