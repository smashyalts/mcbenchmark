#!/usr/bin/env bash
# Alternating with/without A/B for the BenchCapture plugin.
#
# Two things this guards against, both learned the hard way:
#
#  1. Ordering effects. A server that has already generated terrain ticks faster
#     than one that has not, and connects more players, so "run A then run B"
#     systematically favours whichever ran second. Arms therefore alternate, and
#     you should read the spread across repeats rather than any single pair.
#
#  2. Overlapping instances. Every run shares one server directory and one
#     console pipe, so two copies of this script silently corrupt each other's
#     results. A lock directory makes the second copy refuse to start.
#
# Note on what this can and cannot show: the plugin costs ~0.05% of main-thread
# samples, while server tick times vary by tens of percent run to run. This
# comparison is a guardrail against a gross regression, NOT a measurement of the
# plugin's cost — for that, use the profiler attribution (host/jfr-attribute.py).
set -u

SRV="${SRV:?set SRV to the test server directory}"
SP="${SP:?set SP to a scratch directory}"
MC="${MC:-$(cd "$(dirname "$0")/.." && pwd)}"
SCENARIO="${SCENARIO:-$SP/move250.yaml}"
REPEATS="${REPEATS:-2}"

LOCK="$SRV/.ab.lock"
if ! mkdir "$LOCK" 2>/dev/null; then
  echo "another A/B run holds $LOCK — refusing to start a second one" >&2
  exit 1
fi
cleanup() {
  taskkill //F //IM java.exe >/dev/null 2>&1
  pkill -f "tail -f console-in" >/dev/null 2>&1
  rmdir "$LOCK" 2>/dev/null
}
trap cleanup EXIT

run_one() {
  local arm="$1" idx="$2"
  if [ "$arm" = "with" ]; then
    [ -f "$SRV/BenchCapture-DISABLED.jar" ] && mv "$SRV/BenchCapture-DISABLED.jar" "$SRV/plugins/BenchCapture-1.0.0.jar"
  else
    [ -f "$SRV/plugins/BenchCapture-1.0.0.jar" ] && mv "$SRV/plugins/BenchCapture-1.0.0.jar" "$SRV/BenchCapture-DISABLED.jar"
  fi

  local log="ab-$arm-$idx.log"
  : > "$SRV/console-in.txt"
  ( cd "$SRV" && nohup bash -c "tail -f console-in.txt | java -Xms4G -Xmx8G -jar paper.jar --nogui > $log 2>&1" >/dev/null 2>&1 & )
  until grep -q "Done (" "$SRV/$log" 2>/dev/null; do sleep 3; done

  "$MC/bin/mc-replay.exe" --scenario "$SCENARIO" --out-dir "$SP/abruns-$arm-$idx" \
      > "$SP/ab-$arm-$idx-replay.log" 2>&1 &
  local rp=$!
  sleep 120   # reach steady state before sampling
  # `local s`: an earlier version reused the caller's `i`, so two runs collapsed
  # onto one log file and one arm was silently lost.
  local s
  for s in 1 2 3 4 5 6; do printf "spark tps\n" >> "$SRV/console-in.txt"; sleep 5; done
  wait $rp

  printf "stop\n" >> "$SRV/console-in.txt"; sleep 14
  taskkill //F //IM java.exe >/dev/null 2>&1
  pkill -f "tail -f console-in" >/dev/null 2>&1
  sleep 3
  echo "done $arm-$idx"
}

for r in $(seq 1 "$REPEATS"); do
  run_one without "$r"
  run_one with "$r"
done
echo "A/B finished"
