#!/usr/bin/env python3
"""Attribute JFR execution samples and allocations to a package prefix.

Answers the question spark's profiler would answer, but locally: of all the
samples taken on the server main thread, what fraction has our plugin anywhere
in the stack? A plugin that is a bottleneck shows up in a meaningful share of
main-thread samples; one that is noise does not.

Counting "anywhere in the stack" is deliberately generous to the plugin's
apparent cost — it attributes a sample to us even when we merely appear below
Bukkit's event dispatch, so the number is an upper bound rather than a flattering
one.

    python host/jfr-attribute.py <recording.jfr> [package-prefix] [thread-name]
"""
import collections
import re
import subprocess
import sys


def jfr_print(path, event):
    # --stack-depth matters: jfr print truncates stacks to a handful of frames by
    # default, which would hide a plugin that sits deep under Bukkit's dispatch
    # and make it look free.
    out = subprocess.run(
        ["jfr", "print", "--stack-depth", "128", "--events", event, path],
        capture_output=True, text=True, errors="replace",
    )
    if out.returncode != 0:
        raise SystemExit(f"jfr print failed: {out.stderr[:500]}")
    return out.stdout


def split_events(text):
    """Yield one event block at a time from `jfr print` output.

    Events always begin at column 0 with the event type name, and everything
    belonging to them is indented, so splitting on that boundary is enough — no
    brace matching needed.
    """
    block = []
    for line in text.splitlines():
        if line.startswith("jdk.") and line.rstrip().endswith("{"):
            if block:
                yield "\n".join(block)
            block = [line]
        elif block:
            block.append(line)
    if block:
        yield "\n".join(block)


THREAD_RE = re.compile(r'sampledThread\s*=\s*"([^"]*)"')
FRAME_RE = re.compile(r"^\s{4,}([\w.$]+\.[\w<>$]+)\(", re.MULTILINE)


def analyse(path, prefix, thread_filter):
    samples = jfr_print(path, "jdk.ExecutionSample")
    total = hits = 0
    top_frames = collections.Counter()
    our_frames = collections.Counter()

    for block in split_events(samples):
        m = THREAD_RE.search(block)
        name = m.group(1) if m else "?"
        if thread_filter and name != thread_filter:
            continue
        total += 1
        # The first "at " line after stackTrace is the innermost frame.
        frames = FRAME_RE.findall(block)
        if frames:
            top_frames[frames[0]] += 1
        if any(prefix in f for f in frames):
            hits += 1
            for f in frames:
                if prefix in f:
                    our_frames[f] += 1
                    break

    print(f"=== jdk.ExecutionSample  thread={thread_filter or 'ALL'} ===")
    print(f"total samples: {total}")
    pct = (100.0 * hits / total) if total else 0.0
    print(f"samples with '{prefix}' anywhere in stack: {hits}  ({pct:.3f}%)")
    if our_frames:
        print("  our frames:")
        for f, c in our_frames.most_common(8):
            print(f"    {c:6d}  {f}")
    print("\ntop main-thread frames (where the time actually goes):")
    for f, c in top_frames.most_common(15):
        print(f"  {100.0*c/total:6.2f}%  {f}")

    # Allocation pressure: GC pauses are stop-the-world, so garbage created on a
    # background thread still costs the main thread. Attribute it too.
    try:
        alloc = jfr_print(path, "jdk.ObjectAllocationSample")
    except SystemExit:
        return
    a_total = a_ours = 0
    a_by = collections.Counter()
    for block in split_events(alloc):
        w = re.search(r"weight\s*=\s*([\d,.]+)\s*(\w+)", block)
        size = 0.0
        if w:
            val = float(w.group(1).replace(",", ""))
            unit = w.group(2).lower()
            size = val * {"bytes": 1, "kb": 1e3, "mb": 1e6, "gb": 1e9}.get(unit, 1)
        a_total += size
        frames = FRAME_RE.findall(block)
        if any(prefix in f for f in frames):
            a_ours += size
            for f in frames:
                if prefix in f:
                    a_by[f] += size
                    break
    if a_total:
        print(f"\n=== jdk.ObjectAllocationSample (all threads) ===")
        print(f"total sampled allocation: {a_total/1e9:.2f} GB")
        print(f"attributed to '{prefix}': {a_ours/1e9:.3f} GB  ({100.0*a_ours/a_total:.3f}%)")
        for f, s in a_by.most_common(5):
            print(f"    {s/1e6:9.1f} MB  {f}")


if __name__ == "__main__":
    if len(sys.argv) < 2:
        raise SystemExit(__doc__)
    analyse(
        sys.argv[1],
        sys.argv[2] if len(sys.argv) > 2 else "com.mcbench",
        sys.argv[3] if len(sys.argv) > 3 else "Server thread",
    )
