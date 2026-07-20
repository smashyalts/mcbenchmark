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
| **bench-playerdata** | `go/cmd/bench-playerdata` | Go | Places bench accounts at their trace's captured position before they log in |
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
go build -o ../bin/trace-compiler   ./cmd/trace-compiler
go build -o ../bin/mc-replay        ./cmd/mc-replay
go build -o ../bin/bench-playerdata ./cmd/bench-playerdata
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

A capture file that ends mid-frame — the normal result of killing a server, or
of a full disk — costs only its own incomplete tail. The compiler keeps every
complete frame in that file and every other file in the directory, and says what
it lost:

```
WARNING: raw-20260720-210921-s0.bin: frame 47: capture file ends mid-frame;
         keeping the events read before that point
```

### 4. Place the bench accounts (benchmark server, stopped)

```bash
bin/bench-playerdata \
  --world  /path/to/benchmark-server/world \
  --manifest host/traces-export/protocol-769/mixed-1h-benchmark/manifest.json \
  --prefix BENCH_ --count 500      # must match the scenario's target_players
```

`--prefix` must match the scenario's `identity.username_prefix` exactly, and both
cap it at **11 characters** — the account index adds five digits and Minecraft
stops at sixteen. A longer prefix used to be truncated on both sides, which does
not rename the accounts so much as merge them: every session logs in as the same
player, the server kicks each new one as a duplicate login, and the run reports a
wall of failed sessions with no stated cause. It is now refused up front.

Skipping this step does not fail loudly — it fails quietly, which is worse. A
bench account that has never logged in spawns at **world spawn**, so:

- every dig and place is out of interaction range and the server drops it, while
  the run still reports the events as replayed and the world never changes; and
- if world spawn is not solid ground the bot hovers, and the server kicks it with
  *"Flying is not enabled on this server"* after four seconds.

**For true 1:1 gear, hand the bots the real players' data.** Export the
production server's player data directory and pass it as `--import`; each trace
is matched back to the player who produced it and that player's file is copied
onto the bench account, with only the UUID and position rewritten:

```bash
bin/bench-playerdata --world /path/to/benchmark-server/world   --manifest .../manifest.json --count 500   --import /path/to/production/world/players/data
```

This carries everything the inventory snapshot cannot: enchantments, durability,
XP, hunger, the ender chest, abilities. Verified on Paper 26.1.2 — an imported
bot logged in with `XpLevel: 42` and the source player's diamond chestplate.

Matching needs the capture's player id, which is `SHA-256(uuid || salt)`. With
`anonymize_players: false` the salt is sixteen zero bytes, so the tool
reproduces the hash from each `.dat` filename and matches exactly — verified
byte-for-byte against an independently computed digest. With anonymisation on
the salt is random and discarded by design, so pass `--player-map` (lines of
`hash uuid`) exported at capture time, or accept arbitrary assignment. The tool
reports `N matched to their own player, M filled from leftovers` and warns loudly
when nothing matched, because arbitrary gear still produces a plausible-looking
run.

It also arms them: the capture plugin records each player's inventory at login,
and `bench-playerdata` writes those stacks into the bot's player data. This
matters more than it sounds — a replay client cannot give itself items, and tool
tier dominates block-break time (barehanded stone is 7.5 s against a diamond
pickaxe's 0.4 s), so a mining trace replayed empty-handed produces a bot swinging
at blocks that never break. Captures taken before `capture_inventory` existed
carry none, and the tool says so.

**If you restore a world backup between runs, restore first and place second.**
Player data lives *inside* the world folder (`world/players/data`), so rolling
the world back to a pristine state also deletes the accounts you placed. The bot
then spawns at world spawn — which in a fresh world is a handful of blocks from
where the capture happened, close enough to look right on screen and still
outside the ~4.5 block interaction range. Measured: a bot 6 blocks off broke 0
of 12 blocks while swinging the whole time.

Run it with the server **stopped, before every run**. Paper reads player data at
login and writes it back at logout, so a file written under a running server is
ignored, and after a run each account's data holds wherever that bot finished —
the next run would start from there instead of the captured origin. `--remove`
deletes the files again; `--dry-run` shows the placements without writing. The tool auto-detects where this server version
keeps player data (`world/players/data` on current versions,
`world/playerdata` on older ones) and prints which one it chose.

### 5. Replay (host, against offline-mode benchmark server)

Edit a scenario under `host/scenarios/`, then:

```bash
bin/mc-replay --scenario host/scenarios/1h-default.yaml --out-dir host/runs/$(date +%F-%H%M)
# or drive it with the orchestrator:
host/bench-runner.sh host/scenarios/1h-default.yaml <server-host> <server-port>
```

The run writes `run.json` (peak concurrency, per-session results, a 5-second
concurrency time series) and `metrics.prom` (Prometheus text) to the out dir.

**Read `digs_confirmed` and `places_confirmed`, not `events_replayed`.** Sending
a block packet proves nothing: the server silently drops a dig that is out of
range or aimed at air, and a placement whose target is not a solid block to build
against, while `events_replayed` counts them either way — so a run that changed
nothing looks identical to one that worked. The confirmed counters come from the
`block_update` packets the server sent back, and the live log prints them as
`confirmed/sent`:

```
active=1 connected=1 failed=0 events=124 digs=12/12 places=4/4   <- working
active=1 connected=1 failed=0 events=124 digs=0/12  places=0/4   <- sent, world unchanged
```

A run also warns at login when the server put a bot somewhere other than where
its trace was captured, which is the usual reason for `digs=0/N`:

```
WARNING: BENCH_00000 spawned at (-807.5, 78.0, -51.5) but its trace was captured
at (5.5, 71.0, 6.5), 815 blocks away. Block events will be out of range and do
nothing. Run bench-playerdata ... before every run.
```

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
- **Blocks actually break**: a replayed dig removed the target block, confirmed
  by `execute if block <pos> minecraft:air` after the run. The A/B is what makes
  it meaningful — the previous finish-only packet sequence left the block
  standing (`Test failed`), the start+finish sequence removes it (`Test passed`).
- **Teleports don't corrupt the delta chain**: a bot was captured, `/tp`'d 1700
  blocks mid-session, and the capture recorded one `REANCHOR` at exactly the
  destination the server reported — with the largest movement delta in the whole
  file still 0.35 blocks, i.e. no bogus jump leaked into the movement stream.
- **Bots spawn where they were captured**: with `bench-playerdata` run first, the
  server logged `BENCH_00000 logged in ... at (-904.5, 79.0, -152.5)` — the
  trace's origin — instead of world spawn, and a dig 100 blocks from spawn then
  broke its block. Without it, the same trace left the block untouched.
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

- Server-initiated teleports can't be forced from the client. Capture records
  them (`REANCHOR`) and keeps the delta chain correct across them, but a bot
  claiming a position 1600 blocks away is indistinguishable from cheating and is
  rejected. The bot follows only if the benchmark server teleports it too — the
  captured command replaying, or the same portal. Whatever is left is reported as
  `relocations_unreproduced` in `run.json` rather than silently faked.
- The replay client does not simulate gravity: it sends the positions the trace
  recorded and never falls on its own. A bot that starts in mid-air stays there
  and is kicked as "flying" after four seconds. In practice this only bites when
  a bot is placed badly — see step 4 — because captured movement carries the
  player's real descent.
- Real captured survival movement replays faithfully, but synthetic *airborne*
  (creative-flight) travel is throttled by the server's flight anti-cheat — use
  `gen-demo --tp` (operator teleport) to force long-distance chunk generation.
- Entity-targeted combat can't be replayed verbatim (server-assigned entity IDs),
  so combat replays as a swing. Inventory item-movement fidelity is approximate
  (the server processes the click and resyncs); the container interaction itself
  is reproduced. Unmapped events are counted under `events_skipped` so coverage
  is never overstated.
