# Scaling: from a small capture to a large fleet

The goal is replaying ~1500 concurrent users. You do not need 1500 real players
to get there — record a handful, then **amplify**.

## Why amplification (not cloning)

Replaying one captured trace 300 times gives 300 clones that act at the same
instant, in the same place, with the same values. That is not a realistic load:
it under-tests contention and over-tests one chunk. `trace-amplify` rewrites each
copy along three axes:

| Axis | What it varies | Why it matters |
|------|----------------|----------------|
| **time** | per-trace start delay + per-event jitter | clones desync instead of firing in lockstep |
| **space** | per-trace block offset on absolute coords (dig/place/container pos) | players spread across chunks instead of stacking |
| **values** | integer literals in commands (±%) | distinct auction prices, amounts, rows — not one repeated value |

Usernames need no rewriting: traces reference `{SELF}`, which the replay client
expands to each session's own username at send time. That is what decouples trace
content from trace→player assignment, so any trace can run as any player.

```bash
# record 5 players -> compile -> amplify to 1500
trace-compiler --input capture-logs --output traces/ --min-duration 60 --protocol 775
trace-amplify  --in traces/manifest.json --out fleet/ --count 1500 \
    --seed 7 --start-jitter-s 120 --event-jitter-ms 250 \
    --space-spread 512 --vary-numbers-pct 25
```

Same seed + same inputs = identical output, so runs are reproducible.

## Measured results (Paper 26.1.2, auction-house workload)

Both runs used the NexusAuctionHouse buy/sell flow with the BenchCapture plugin
active, on one machine (server + replay client co-located).

| | 50 users | 100 users (amplified 2→100) |
|---|---|---|
| Sessions connected / failed | 50 / **0** | 100 / **0** |
| Capture events | 5,871 | 7,572 |
| Capture throughput | ~56 ev/s | ~50 ev/s |
| **Events dropped** | **0** | **0** |
| Server overload warnings | **0** | **0** |
| Plugin commands processed | 906 | 1,080 |
| Capture size | 146 KiB | 197 KiB |
| Distinct AH prices | 1 | **33** |
| Distinct funding amounts | 1 | **50** |

The capture pipeline had ample headroom at both sizes: zero drops, zero server
overload warnings, and the capture logs recompiled back into replayable traces.

## Projecting to 1500 — and what is still unmeasured

**This workload** is time-sparse: ~0.5 capture-events/sec per player. Straight
extrapolation to 1500 users gives roughly **750 ev/s and ~1–2 MiB/min** of
compressed capture, which is well within what the async writer handled here.

**The movement-heavy case has now been measured** — see
[CAPTURE-COST.md](CAPTURE-COST.md) for the full write-up. `PlayerMoveEvent` fires
at most once per player per tick, so 1500 traversing players top out around
30,000 events/sec. At that rate capture costs **0.41 ms of the 50 ms tick
(0.82%) with zero allocation**, measured against the real capture path.

That measurement also turned up four defects, all now fixed: an O(players) scan
per mob event, a per-event clock read that dominated the hot path, a
zero-allocation buffer layout that was slower than the allocating one it
replaced, and a flush schedule tied to server ticks that dropped events precisely
when the server was struggling.

Known scaling characteristics to watch:

- **Main-thread cost is bounded but not zero.** Bukkit dispatches events on the
  main thread, so capture cannot avoid it entirely; it can only make the work
  trivial. Encoding, compression and IO all happen on the writer thread.
- **Backpressure is visible, not silent.** Each player has a bounded ring. If the
  writer lags, events are dropped and counted — `capture stats: ... dropped=N`
  and the summary on disable. A non-zero drop count means capture is the
  bottleneck, not the server.
- **Read `tps=` before trusting an event count.** The stats line reports the
  server's measured tick rate. A 250-player run captured only ~0.5 movement
  events/sec/player not because capture lost anything, but because the server was
  at 7 TPS and genuinely was not processing movement faster. Paper logs no
  warning until a tick overruns by two seconds, so an unhealthy server can look
  quiet.
- **Hard kills lose the last flush.** `SIGKILL`/`taskkill /F` skips `onDisable`,
  so up to one flush interval (default 1s) of buffered events is lost and no
  summary is logged. Complete frames already on disk stay readable — a capture
  file from a hard-killed server decoded and recompiled fine.
- **Client side.** 100 sessions from a single replay process sent 20,340 packets
  / 233 KB. At 1500, expect to tune file-descriptor limits and possibly shard the
  replay client across machines; `connect_per_second` also gates ramp-up.

## Reproducing

```bash
bin/gen-demo      --output ah-traces --ah --players 2 --price 100
bin/trace-amplify --in ah-traces/manifest.json --out fleet --count 100 --vary-numbers-pct 30
bin/mc-replay     --scenario host/scenarios/nexus-ah.yaml --out-dir runs/
```

Op the demo users (offline UUIDs are deterministic from the username) and run the
server in creative offline-mode — see [PLUGIN-LOADTEST.md](PLUGIN-LOADTEST.md).
