# Minecraft Benchmark System

Capture real player behavior on a production Paper server, compile it into
replayable traces, and replay hundreds of virtual players against an offline
benchmark server for up to an hour per run.

Targets **Minecraft 26.1.2 (protocol 775)**; validated end-to-end against a real
Paper 26.1.2 server (see [Validation](#validation)). Retarget another version by
regenerating packet IDs (see [docs/PROTOCOL.md](docs/PROTOCOL.md)).

```
 ┌────────────────────────┐   raw-*.bin    ┌─────────────────┐   trace-*.bin   ┌──────────────┐
 │ Paper capture plugin   │ ─────────────▶ │ trace-compiler  │ ──────────────▶ │  mc-replay   │
 │ (online-mode, in Docker)│  capture-logs │ (Go CLI)        │  traces-export  │  (Go CLI)    │
 └────────────────────────┘                └─────────────────┘                 └──────┬───────┘
        records movement,                    sessionizes,                             │ Java protocol 769
        commands, combat,                    normalizes timing,                       ▼
        inventory, mobs                       tags sessions                    ┌──────────────┐
                                                                               │ offline-mode │
   copy capture-logs/ or traces-export/ out of the container ────────────────▶│ Paper server │
                                                                               └──────────────┘
```

## Components

| Component | Path | Language | Role |
|-----------|------|----------|------|
| **Capture plugin** | `capture-plugin/` | Java 21 (Paper API) | Records events to `raw-*.bin` logs inside the container |
| **trace-compiler** | `go/cmd/trace-compiler` | Go | Compiles capture logs into per-session traces + manifest |
| **mc-replay** | `go/cmd/mc-replay` | Go | Replays traces as virtual players against the benchmark server |
| **bench-runner** | `host/bench-runner.sh` | bash | Optional orchestrator: wait-for-server → replay → archive |

| **trace-amplify** | `go/cmd/trace-amplify` | Go | Synthesizes many varied sessions from a small real capture (record 5 → replay 1500) |

Dev/verification tools: `gen-fixture` (synthetic capture logs), `gen-demo`
(targeted demo traces incl. the auction-house flow), `mock-server` (minimal
server to smoke-test the replay binary), `dump-capture` (decode capture logs),
`interop-check` (proves Java↔Go byte compatibility).

Scaling a small capture up to a large fleet is covered in
[docs/SCALING.md](docs/SCALING.md); what the capture plugin costs the server main
thread, and how that was measured, in [docs/CAPTURE-COST.md](docs/CAPTURE-COST.md);
plugin load testing in [docs/PLUGIN-LOADTEST.md](docs/PLUGIN-LOADTEST.md).

The binary formats crossing the Java/Go boundary are specified in
[docs/FORMAT.md](docs/FORMAT.md); the replay protocol coverage is in
[docs/PROTOCOL.md](docs/PROTOCOL.md).

## Build

**Go tools** (needs Go ≥ 1.25):

```bash
cd go
go build -o ../bin/trace-compiler ./cmd/trace-compiler
go build -o ../bin/mc-replay      ./cmd/mc-replay
go build -o ../bin/gen-fixture    ./cmd/gen-fixture
go build -o ../bin/mock-server    ./cmd/mock-server
go test ./...
```

**Capture plugin** (needs JDK ≥ 21). With Maven: `cd capture-plugin && mvn package`.
Without Maven, a plain-`javac` build script fetches the compile deps and packages
the jar:

```bash
cd capture-plugin
./build.sh          # produces BenchCapture-1.0.0.jar
```

## Workflow

### 1. Capture (production, online-mode server)

Drop `BenchCapture-1.0.0.jar` into the Paper server's `plugins/`. It writes
`raw-*.bin` logs to the `output_path` in `config.yml`
(default `/home/container/bench-capture/capture-logs`). Player UUIDs are hashed
with a per-run salt before anything touches disk.

### 2. Export

Copy the capture logs (or, after step 3, the compiled traces) out of the
container to the host, e.g.:

```bash
docker cp <container>:/home/container/bench-capture/traces-export ./host/traces-export
```

### 3. Compile traces

```bash
bin/trace-compiler \
  --input  /path/to/capture-logs \
  --output host/traces-export/protocol-769/mixed-1h-benchmark \
  --protocol 769 --world-profile bench-arena-v1 \
  --min-duration 600 --max-duration 3600 --run-id 2026-07-18-2355
```

### 4. Replay (host, against offline-mode benchmark server)

Edit a scenario under `host/scenarios/`, then:

```bash
bin/mc-replay --scenario host/scenarios/1h-default.yaml --out-dir host/runs/$(date +%F-%H%M)
# or drive it with the orchestrator:
host/bench-runner.sh host/scenarios/1h-default.yaml <server-host> <server-port>
```

The run writes `run.json` (peak concurrency, per-session results, a 5-second
concurrency time series) and `metrics.prom` (Prometheus text) to the out dir.

## Try it without a server

The whole pipeline runs offline against synthetic data:

```bash
# 1. synthesize a capture log and compile it
bin/gen-fixture --output /tmp/cap --players 4 --minutes 25 --seed 3
bin/trace-compiler --input /tmp/cap \
  --output host/traces-export/protocol-769/mixed-1h-benchmark \
  --protocol 769 --world-profile bench-arena-v1 --min-duration 600 --run-id demo

# 2. replay against the mock server
bin/mock-server --addr 127.0.0.1:25577 &
bin/mc-replay --scenario host/scenarios/smoke-local.yaml --out-dir /tmp/run
```

`host/traces-export/…/` already contains a compiled demo trace set produced this
way, so `mc-replay` has something to load out of the box.

## Validation

Validated against a **real Paper 26.1.2 server** (offline-mode) running on this
machine, in addition to unit/integration tests:

- **Live server, protocol 775**: `mc-replay` connected virtual players that the
  server logged joining with the correct offline UUIDs, with **zero protocol
  errors** across full runs. Packet IDs were extracted from the server's own
  data generator.
- **Movement processed**: the server's `walk_one_cm` player statistic
  incremented — that counter only advances when position packets are accepted.
- **Commands processed**: server logged replayed `/say` and `/tp` commands
  executing.
- **Chunk/region generation from activity**: op'd players replaying `/tp` to
  ±1600-block waypoints grew the overworld region files from **4 (spawn) to 26**,
  with new `.mca` files at exactly the teleport destinations (e.g. `r.3.3` at
  1600,1600).
- **Player NBT persisted**: player `.dat` files were written with the flown/
  teleported position and, via replayed creative-set packets, a populated
  **Inventory** (diamonds, diamond blocks, golden apples) — the server even
  awarded the `[Diamonds!]` advancement.
- **Inventory reproduced with live window IDs**: `INV_CLICK`/`INV_OPEN`/
  `INV_CLOSE` drive Container Click/Open/Close using the window & state ids the
  server assigns at replay time (see [docs/PROTOCOL.md](docs/PROTOCOL.md)).

Also: `go vet` clean; unit tests for VarInt/VarLong golden vectors and all format
round-trips; a replay integration test that drives a full session over a real TCP
socket in both compressed and uncompressed framing; and `interop-check`, which
proves the Java plugin's encoding classes + zlib produce capture logs the Go
reader decodes field-for-field. The plugin compiles against `paper-api` and
packages to `BenchCapture-1.0.0.jar`.

Reproduce the live validation quickly:

```bash
bin/gen-demo --output /tmp/demo --players 4 --creative --tp   # teleport + creative fill
# (server must be offline+creative, players op'd; see docs/PROTOCOL.md)
bin/mc-replay --scenario host/scenarios/... --out-dir /tmp/run
```

- **Capture cost on the main thread**: 0.41 ms per tick (0.82% of the 50 ms
  budget) with **zero allocation** at 1500 simulated players moving at 20
  events/sec — measured against the real capture path, with a live 250-player
  movement run on Paper 26.1.2 confirming zero drops and a clean recompile back
  into 250 traces. Details and method in [docs/CAPTURE-COST.md](docs/CAPTURE-COST.md).

### Known limitations (by design)

- Movement is captured per Bukkit event, not per packet — sufficient for load
  characterization; a packet-level source (ProtocolLib/PacketEvents) can be
  slotted in behind `CaptureManager` without changing the format. That would also
  remove the main-thread hop entirely, since Bukkit events can only be received
  on the main thread.
- Real captured survival movement replays faithfully, but synthetic *airborne*
  (creative-flight) travel is throttled by the server's flight anti-cheat — use
  `gen-demo --tp` (operator teleport) to force long-distance chunk generation.
- Entity-targeted combat can't be replayed verbatim (server-assigned entity IDs),
  so combat replays as a swing. Inventory item-movement fidelity is approximate
  (the server processes the click and resyncs); the container interaction itself
  is reproduced. Unmapped events are counted under `events_skipped` so coverage
  is never overstated.
