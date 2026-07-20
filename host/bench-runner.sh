#!/usr/bin/env bash
# Component D: host-side orchestrator. Waits for the offline benchmark server to
# accept connections, runs mc-replay against a scenario, and archives the run.
#
# It does NOT start/stop the server itself by default (uncomment the hooks below
# if you manage the server via systemd/docker on this host). It is deliberately
# unaware of Pterodactyl internals.
#
# Usage: bench-runner.sh <scenario.yaml> [host] [port]
set -euo pipefail
cd "$(dirname "$0")"

SCENARIO="${1:?usage: bench-runner.sh <scenario.yaml> [host] [port]}"
HOST="${2:-127.0.0.1}"
PORT="${3:-25565}"
TS="$(date +%F-%H%M%S)"
OUT="runs/${TS}"
MCREPLAY="${MCREPLAY:-mc-replay}"   # path to the built mc-replay binary

mkdir -p "$OUT"

# --- optional: start the offline benchmark server here ---
# systemctl start mc-bench-server
# docker start mc-bench-server

echo "[bench-runner] waiting for ${HOST}:${PORT} ..."
for i in $(seq 1 60); do
  if (exec 3<>"/dev/tcp/${HOST}/${PORT}") 2>/dev/null; then
    exec 3>&- 3<&-
    echo "[bench-runner] server is up"
    break
  fi
  sleep 1
  if [ "$i" -eq 60 ]; then
    echo "[bench-runner] server did not open ${HOST}:${PORT} within 60s" >&2
    exit 1
  fi
done

echo "[bench-runner] running scenario ${SCENARIO} -> ${OUT}"
"$MCREPLAY" --scenario "$SCENARIO" --out-dir "$OUT"

# --- optional: stop the server here ---
# systemctl stop mc-bench-server
# docker stop mc-bench-server

echo "[bench-runner] run complete. Report: ${OUT}/run.json"
