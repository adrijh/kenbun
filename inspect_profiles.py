#!/usr/bin/env python3
"""Inspect OTLP profiles JSONL written by the collector's file exporter.

Usage:
  inspect_profiles.py data/profiles.jsonl                 # processes by sample count
  inspect_profiles.py data/profiles.jsonl -p python3.12   # top functions for one process
  inspect_profiles.py data/profiles.jsonl -p python3.12 --stacks 5
"""
import argparse
import collections
import json


def resolve(d):
    st = d["stringTable"]
    fns = d.get("functionTable", [])
    locs = d.get("locationTable", [])
    stacks = d.get("stackTable", [])
    maps = d.get("mappingTable", [])

    def s(i):
        return st[i] if i is not None and i < len(st) else ""

    def attrs(kvs):
        out = {}
        for kv in kvs:
            key = kv.get("key") or s(kv.get("keyStrindex", 0))
            v = kv.get("value", {})
            # strings can be inline or a string-table reference
            if "stringValueStrindex" in v:
                out[key] = s(v["stringValueStrindex"])
            else:
                out[key] = next(iter(v.values()), None)
        return out

    def frames(stack_idx):
        """Function names, leaf first."""
        out = []
        for li in stacks[stack_idx].get("locationIndices", []):
            loc = locs[li]
            lines = loc.get("lines", [])
            if not lines:
                m = maps[loc.get("mappingIndex", 0)] if maps else {}
                out.append(f"?? ({s(m.get('filenameStrindex', 0)) or 'unknown'})")
                continue
            for line in lines:
                out.append(s(fns[line.get("functionIndex", 0)].get("nameStrindex", 0)) or "??")
        return out

    return attrs, frames


def weight(sample):
    vals = sample.get("values")
    if vals:
        return sum(int(v) for v in vals)
    return len(sample.get("timestampsUnixNano", [])) or 1


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("file")
    ap.add_argument("-p", "--process", help="process.executable.name to drill into")
    ap.add_argument("-n", type=int, default=15, help="rows to show")
    ap.add_argument("--stacks", type=int, default=0, help="also print the N heaviest full stacks")
    args = ap.parse_args()

    per_proc = collections.Counter()
    self_ = collections.Counter()
    total = collections.Counter()
    full = collections.Counter()

    with open(args.file) as f:
        for line in f:
            if not line.strip():
                continue
            batch = json.loads(line)
            attrs, frames = resolve(batch["dictionary"])
            for rp in batch.get("resourceProfiles", []):
                ra = attrs(rp.get("resource", {}).get("attributes", []))
                exe = ra.get("process.executable.name") or f"pid:{ra.get('process.pid', '?')}"
                for sp in rp.get("scopeProfiles", []):
                    for prof in sp.get("profiles", []):
                        for smp in prof.get("samples", []):
                            w = weight(smp)
                            per_proc[exe] += w
                            if exe != args.process:
                                continue
                            fr = frames(smp.get("stackIndex", 0))
                            if fr:
                                self_[fr[0]] += w
                            for name in set(fr):
                                total[name] += w
                            full[tuple(fr)] += w

    if not args.process:
        print(f"{'samples':>8}  process")
        for exe, n in per_proc.most_common(args.n):
            print(f"{n:>8}  {exe}")
        return

    n_all = per_proc[args.process] or 1
    print(f"{args.process}: {per_proc[args.process]} samples\n")
    print(f"{'self%':>6} {'total%':>7}  function")
    for name, _ in total.most_common(args.n):
        print(f"{100*self_[name]/n_all:6.1f} {100*total[name]/n_all:7.1f}  {name}")

    for stack, w in full.most_common(args.stacks):
        print(f"\n{w} samples:")
        for name in reversed(stack):  # root first
            print(f"  {name}")


if __name__ == "__main__":
    main()
