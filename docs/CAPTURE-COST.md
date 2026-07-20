# What capture costs the server

A capture plugin sits on the critical path of the thing it measures. If it takes
a meaningful slice of the tick, the benchmark is measuring the observer. This
documents what BenchCapture actually costs, how it was measured, and what is
still unknown.

## Where capture runs

**Movement is captured from packets, on Netty threads, and never touches the
main thread.** A serverbound flying packet is read by
`PacketMovementListener` on the connection's own Netty event loop, via
PacketEvents. Measured on a live Paper 26.1.2 server, capture accounts for
**0.07–0.16% of Netty event-loop time**, and those threads are 79–98% idle.

**Everything else is stuck on the main thread**, because Bukkit dispatches its
events there and nowhere else: the event object is only valid during synchronous
dispatch, and touching most of the Bukkit API off-thread is unsafe by design.
That covers commands, inventory, digging, block placement and mob attribution —
kinds that fire a few times per player per minute. Live spark attribution puts
the whole plugin at **0.0000–0.0089% of main-thread time**.

The budget rule is the same on both: read a few primitives, store them into
memory that already exists, publish an index. All wire encoding, compression,
file IO, rotation and deferrable allocation happen on the writer thread.

### Why packets, not `PlayerMoveEvent`

Performance is the smaller half of the argument. `PlayerMoveEvent` is a
filtered, post-validation view, and it hides load the benchmark exists to
reproduce:

- **Rejected movement.** The server pays full receive, decode and validate cost
  for a move it then refuses. No event fires, so an event-built replay
  under-reproduces real load.
- **Idle position updates.** A stationary client still sends flying packets
  every tick. No event fires, so the captured packet rate is understated.
- **Sub-tick timing.** Events collapse onto tick boundaries by construction;
  packets carry their real arrival time.

### Is PacketEvents fast enough, or is raw Netty needed?

Measured, not assumed. Live server, 150 players, 20.0 TPS, zero dropped events,
spark attribution of the four Netty event-loop threads. Run twice, because the
sampler engine matters (see below):

| under | Linux / async-profiler | Windows / Java sampler |
|---|---|---|
| `io.netty` (everything) | 100% | 100% |
| — idle in the epoll/WEPoll wait | 97.8% | 79% |
| `net.minecraft` (vanilla decode/send) | 1.57% | 19% |
| `com.github.retrooper` (all of PacketEvents) | **0.114%** | 0.94% |
| — of which serverbound | 0.072% | 0.26% |
| — of which clientbound | 0.042% | 0.68% |
| `com.mcbench` (our listener) | **0.033%** | 0.16% |

On the trustworthy (async-profiler) numbers, capture is **1/47th of what vanilla
itself spends** on those threads and PacketEvents is **1/14th**, while the
threads sit idle 98% of the time. Main-thread cost is 0.0067%, all of it cold
Bukkit events — no movement.

Raw Netty is not warranted. It would not remove decoding, only move it into this
codebase, along with ownership of wire layouts, handler ordering and channel
injection across every protocol bump — breaking on the update, under load, in
production.

Two things worth noting. PacketEvents spends a third of its time on *clientbound*
packets, which capture wants none of; that is the cost of PacketEvents being
installed at all, not of this plugin. And on the target production server
PacketEvents is already installed for other plugins, so this plugin's marginal
PacketEvents cost is zero.

#### The load generator was sending a third of the real packet rate

Worth stating plainly, because it invalidated the first round of scale numbers: a
vanilla client sends **exactly one movement packet per tick — 20/sec — whether or
not the player moved**, and `mc-replay` only sent movement when the trace had a
movement event. Measured 7.2 events/sec/player against a real 20.

The irony is that this is precisely the traffic packet capture exists to see.
Switching capture from `PlayerMoveEvent` to packets was justified partly by idle
position updates being invisible to events — while the generator producing the
load was not sending them either.

Two defects, both in `protocol_flow.go`:

1. **No idle-tick movement.** A comment claimed "idle-tick movement flag keeps
   the connection live between events" and no code did it.
2. **Traces ended at their last event, not their duration.** `playOnce` looped
   `for idx < len(events)`, so a trace whose events stop early both skipped the
   remaining traffic and replayed faster than real time. `holdUntil` likewise
   parked the socket open while sending nothing.

Now every session emits one movement packet per tick: whatever the trace
supplies, otherwise `move_player_status_only`, with a full position packet forced
every 20 ticks the way the vanilla client re-syncs. Measured after the fix:
**20.05 events/sec/player**, and `TestClientSendsMovementEveryTick` pins it.

#### Scaling: 150 → 550 players

All async-profiler, all 20.0 TPS with zero dropped events, Netty threads 95–98%
idle and the main thread 99% parked throughout — nothing here is measured against
a saturated machine. The last column is the one that counts, since only it has a
realistic per-player packet rate:

| | 150 players | 550 players | **550, realistic rate** |
|---|---|---|---|
| capture event rate | 1,050 ev/s | 3,967 ev/s | **11,012 ev/s** |
| events per player per second | 7.0 | 7.2 | **20.05** |
| `com.mcbench` on Netty threads | 0.033% | 0.170% | **0.370%** |
| `com.github.retrooper` | 0.114% | 0.287% | **0.592%** |
| `net.minecraft` on Netty threads | 1.57% | 1.08% | **1.51%** |
| `com.mcbench` on main thread | 0.0067% | 0.0000% | **0.0000%** |
| Netty threads idle | 96.6% | 96.6% | **94.9%** |

The last two columns are the same player count at different packet rates, which
isolates rate from session count: **2.78x the packets cost 2.18x the time** —
sublinear, as batching on the event loop would predict. That also retires an
earlier reading of this table that looked superlinear; at 80 samples it was mostly
noise.

Extrapolating to 1500 players at a realistic 20 packets/sec each (30,000
packets/sec):

- straight-line on rate: **~1.0%** of four Netty threads
- carrying the observed sublinear exponent (0.76): **~0.8%**

Call it **~1%**, allowing for the session-count effect the middle column hints
at. The writer would see ~30,000 ev/s against a measured 240,000 ev/s
single-thread capacity — 12.5% utilisation, and still the reason
`writer_threads` defaults to 1.

**This is extrapolation, not measurement** — see the ceiling note below.

One calibration worth keeping: `PacketPathBench` reports 0.28% per Netty thread
at 30,000 ev/s, while the live server costs 0.370% at 11,012 ev/s — about **3.6x
more per packet**. The synthetic harness only exercises the ring write; live adds
the PacketEvents wrapper decode, session-map lookups, the spatial-index update and
cache pressure from hundreds of real sessions. Treat `PacketPathBench` as a lower
bound on the real cost, not an estimate of it.

#### What this hardware can actually host

550 concurrent players, and the limit is **RAM, not the plugin and not CPU**: the
box has 15.7 GB total, WSL gets ~7 GB of it, and a 5 GB heap plus the JVM leaves
~1.6 GB. At 800 the server started timing connections out. There is nothing to
give — the Windows side hosting the bots was down to 0.7 GB free.

Worth recording because Linux moved the ceiling a lot: the same box on Windows
collapsed to 0 TPS at 329 players, with the stall in *vanilla*
(`ServerWaypointManager.updateWaypoint` via `handleMovePlayer`, superlinear in
nearby players). On Linux, 550 players held 20.0 TPS with the main thread 99%
idle.

So any claim about behaviour *at* 1500 comes from the synthetic harnesses
(`PacketPathBench`, `MainThreadBench`) or from production, never from a local
run. What a local run does give — per-packet and per-event cost — is
scale-independent and holds.

#### Why the two engine columns differ by ~5x

spark falls back to its own Java sampler when async-profiler is unavailable,
which it is on Windows. That sampler is **safepoint-biased**: it can only sample
threads at safepoint-pollable locations, so it over-attributes Java frames and
cannot see native ones at all. On Linux, async-profiler samples anywhere and
resolves native frames — which is why 97.8% of Netty thread time resolves to
`libc.so.6.epoll_pwait2` there, and why every Java package's share falls
proportionally.

Both engines agree on the ranking and on the conclusion; the Java sampler simply
inflates everything Java by roughly the same factor. The honest reading is that
the Windows column is an **upper bound** and the Linux column is the real cost.
Verify which engine produced a profile before quoting it — it is recorded in the
payload metadata as `engine=ASYNC` or `engine=JAVA`:

```
java -cp "out;spark-paper.jar" Engine <payload>   # see host/, prints engine/mode/os
```

Production is Linux, so the async-profiler column is the one that applies.

## Measured cost

Hardware: Intel Core Ultra 7 255HX, 20 logical cores, Windows 11 in performance
power mode, JDK 25. Power mode matters more than expected: the same benchmarks on
the balanced profile measured roughly 2.4x slower with far more jitter (max
per-tick cost 3.47 ms vs 0.59 ms), so figures taken on a throttled laptop are not
comparable to a server.
Method: `capture-plugin/tools/MainThreadBench.java` drives the real capture path
at a given player count, shaped into 50 ms ticks, with the real `WriterTask`
draining concurrently. Per-event costs come from `tools/ABBench.java`, which
alternates variants inside one JVM — cross-JVM comparison had ±20% swings, which
is larger than most of the effects below.

### Per movement event

| Variant | ns/event | Allocation |
|---|---|---|
| Session map lookup alone (floor) | 4 | 0 B |
| Lookup + `System.nanoTime()` | 28 | 0 B |
| **Original**: payload array + RawEvent + queue node | 60 | 224 B |
| **Current**: packed ring slot, tick-cached clock | **22** | **0 B** |

### Per tick, at scale

Movement at 20 events/sec/player — the worst realistic case, since
`PlayerMoveEvent` fires at most once per player per tick.

| Players | Main-thread ms/tick (mean) | p99 | % of the 50 ms budget | Dropped | Alloc |
|---|---|---|---|---|---|
| 500 | 0.091 | 0.22 | 0.18% | 0 | 0 B |
| **1500** | **0.246** | **0.45** | **0.49%** | 0 | 0 B |
| 3000 | 0.609 | 0.94 | 1.22% | 0 | 0 B |

Before this work the same 1500-player load generated **6.2 MiB/s** of garbage
attributable purely to the observer, and cost roughly twice as much main-thread
time per event.

## What made the difference

### 1. The clock, not the encoding

`System.nanoTime()` measured **24 ns** against a 4 ns floor — the single largest
item in the per-event cost, and more than the entire rest of the work combined.

This drove a per-tick cached timestamp, refreshed by a main-thread task and
shared by every event in that tick. **That cache has since been removed**, and
the history is worth keeping because the reasoning was sound and the conclusion
still expired.

The cache was worth 24 ns per event only because movement — the one high-rate
kind — ran on the main thread. Once movement moved to packets on Netty threads,
the cache was saving 24 ns on events that fire a few times per player per minute,
while charging real accuracy for it: the cached value is stale for as long as a
tick takes, so on a struggling server a command could be stamped hundreds of
milliseconds before movement that actually preceded it, and the compiler sorts by
timestamp. A 150-player capture showed `session_start` markers **360 ms** behind
the first movement of their own session. Every producer now reads the clock
directly; after the change the same marker skew is **≤30 ms**, and bounded by
real work rather than by tick lag.

The lesson generalises: an optimisation justified by one hot path silently
becomes pure cost when that path moves.

### 2. Zero allocation — but only once it fit in cache

The first zero-allocation attempt used parallel arrays (one per field). It
allocated nothing and was **slower than the allocating version it replaced**
(695 ns vs 439 ns): eight arrays meant eight cache lines touched per event, while
the old design's short-lived objects were allocated bump-pointer in a cache-hot
TLAB.

Packing each event into a single 64-byte slot — one cache line — is what made
zero-allocation actually win. Ring size matters for the same reason: at 512 slots
per player the buffers stop fitting in cache and the path measured ~40% slower
than at 32. The ring is now capped at 128 slots (~6 s of buffer at 20 events/sec),
with the configured `buffer_per_player_kb` acting as a ceiling rather than a
target.

The general lesson: *allocation-free* and *fast* are not synonyms. Escape-analysed
TLAB allocation is cheap; a large cold buffer is not automatically an improvement.

### 3. Mob attribution was O(players) per mob event

`CreatureSpawnEvent` and `EntityDeathEvent` carry no player, so capture attributes
them to a nearby player. That was done by scanning `world.getPlayers()`,
allocating a `Location` per player and computing a distance — per mob event.

| Players | Old scan | New spatial index |
|---|---|---|
| 100 | 3.7 µs | — |
| 500 | 9.2 µs | — |
| 1000 | 16.9 µs | — |
| **1500** | **10.8 µs** | **0.14 µs** (76× faster) |

(The scaling column was measured on the balanced power profile, where 1500
players cost 25.3 µs per call; performance mode brings that to 10.8 µs. The shape
— linear in player count — is what matters.)

The cost grows with player count, and mob event rates *also* grow with player
count, so the total is quadratic in server size. At 1500 players and 2000 mob
events/sec that is **1.1 ms/tick — 2.2% of the budget** in performance mode (5%
on a throttled laptop), on top of everything else.

`PlayerIndex` buckets players into 64-block cells, updated in place from the
movement events capture already handles, and a lookup scans the 3×3 cells around
the entity. Cost no longer depends on how many players are online.

### 4. Flush cadence was tied to server ticks

The writer ran via `runTaskTimerAsynchronously`, which — despite being async —
is *scheduled in server ticks*. At 2 TPS a 20-tick flush becomes a 10-second
flush. That couples capture to server health backwards: buffers fill fastest
exactly when they drain slowest.

This was not theoretical. A 250-player movement run **dropped 2,009 events and
wrote 2 frames in 30 seconds** during the join storm, then dropped nothing once
the server recovered and flushes resumed. The writer now runs on a plain
`ScheduledExecutorService` at a fixed wall-clock rate, independent of tick rate.

Controlled comparison — identical scenario, machine and server settings
(`view-distance=8`), only the flush scheduler differs:

| | Bukkit tick scheduler | Wall-clock scheduler |
|---|---|---|
| Sessions connected | 167 / 250 | **235 / 250** |
| **Events dropped** | **2,009** | **0** |
| Frames in the first 30 s | 2 | ~28 |
| Server TPS during ramp | 1–7 | 1–7 |

The strongest single data point: one 10-second window of the fixed run logged
`tps=0.0` — the main thread had stalled outright — and capture still wrote frames
and dropped nothing. Under tick scheduling a 0-TPS server flushes *never*.

## Backpressure is visible, never silent

Each player has a bounded ring. If the writer falls behind, events are **dropped
and counted** rather than growing memory without bound — and the oldest buffered
events are preserved, since a full ring refuses new writes instead of overwriting
unread ones.

The stats line reports everything needed to judge a capture's trustworthiness:

```
capture stats: 25,506 events written (68 ev/s), 121 frames, 550 KiB,
               dropped=2,009, players=167, tps=7.1
```

- `dropped` > 0 → capture was the bottleneck; the trace has holes.
- `OFF-THREAD=` appears only if an event arrived from a non-main thread. It should
  never appear; if it does, some event source is async and its listener needs
  rethinking. Those events are refused rather than written, because the ring is
  single-producer and a second writer would corrupt it.
- `tps` is the server's real tick rate, measured by the plugin's own per-tick
  task. This matters more than it first appears — see below.

## Read the TPS before trusting an event count

A 250-player run showed capture receiving ~0.5 movement events/sec/player when
the replay client was sending 20/sec. Nothing was wrong with capture: the server
was running at **7 TPS**, so it simply was not processing movement any faster.

Paper had logged no warning, because it only complains when a tick overruns by
more than two seconds — a server can sit at a third of its tick rate silently.
Without the `tps` field this looked like dropped capture data; it was in fact an
accurate recording of an unhealthy server.

The general point: **event counts are only interpretable next to the tick rate
that produced them.** "20,000 events captured" means very different things at 20
TPS and at 7.

## Profiled on a live server (spark + JFR)

The harness numbers above measure the plugin in isolation. This section asks the
production question instead: on a real Paper server under load, is BenchCapture
anywhere near the top of the profile?

Setup: Paper 26.1.2, 250-player movement replay, spark's built-in Java engine
(async-profiler has no Windows/amd64 build), plus a JDK Flight Recorder recording
for local method-level attribution.

spark profiles (performance mode, 250 players):
[with plugin](https://spark.lucko.me/niufnXZPZv) ·
[without plugin](https://spark.lucko.me/fvvcf5uZ8f)

### Where main-thread time actually goes

Measured with spark itself. `host/SparkAttribute.java` parses the uploaded
profile using **spark's own protobuf classes**, taken from the `spark-paper` jar
Paper bundles, so the numbers come from spark's data structures rather than a
reinterpretation of them:

```bash
J=server/libraries/me/lucko/spark-paper/1.10.152/spark-paper-1.10.152.jar
curl -s https://bytebin.lucko.me/<profile-key> -o profile.bin
javac -cp "$J" -d out host/SparkAttribute.java
java  -cp "out;$J" SparkAttribute profile.bin com.mcbench "Server thread"
```

| spark, `Server thread`, 250 players | With plugin | Without plugin |
|---|---|---|
| Total inclusive time | 262,296 | 264,596 |
| **Time under `com.mcbench`** | **68 (0.0259%)** | **0 (0.0000%)** |
| `CaptureListener.onInvClick` | 0.0122% | — |
| `CaptureListener.onJoin` | 0.0091% | — |
| `CaptureListener.onQuit` | 0.0030% | — |
| per-tick clock refresh | 0.0015% | — |

`onMove` — the hottest handler, running up to 20x/sec/player — did not accumulate
enough time to appear at all. What does show up is join/quit, which are rare but
individually heavier (SHA-256 of the player id, ring allocation).

A sanity check on the parser: `net.minecraft` on the same thread returns exactly
100%, as it must (the whole main-thread tree is rooted in the server loop), and
the no-plugin control returns exactly 0%.

**The capture writer thread is idle 99.9% of the time** — `Unsafe.park` accounts
for 99.893% of it, with the actual work (drain, deflate, file write) summing to
about 0.1%. The asynchronous half of the design is nowhere near saturated.

#### Corroborated by JFR

JDK Flight Recorder, sampling independently on the same workload, agrees:

| | With plugin | Without plugin |
|---|---|---|
| Samples on main thread | 20,289 | 20,797 |
| **Samples containing `com.mcbench`** | **1 (0.005%)** | 0 (0.000%) |
| `java.util.HashMap.getNode` | 20.90% | 19.33% |
| `Object2ObjectOpenHashMap.get` | 8.82% | 8.78% |
| `LongOpenHashSet.contains` | 6.51% | 4.60% |
| `ChunkMap$TrackedEntity.updatePlayer` | 4.06% | 4.44% |
| `WaypointTransmitter…isBroken` | 2.70% | 2.69% |

The two profiles are the same shape. The server's cost is entity tracking,
player-map lookups and waypoint transmission — vanilla per-player work that grows
with player count. Capture does not appear in the top 15 frames in either run,
and lands at **1 sample in 20,289**.

That figure is an upper bound, not a flattering number: the attribution counts a
sample if `com.mcbench` appears *anywhere* in the stack, including frames sitting
under Bukkit's event dispatch that would have run regardless.

### Allocation, because GC pauses stop the main thread

Moving allocation to the writer thread does not make it free — a G1 young
collection is stop-the-world, so garbage created anywhere still pauses the tick.
Worth checking explicitly:

| | With plugin | Without plugin |
|---|---|---|
| Sampled allocation (all threads) | 687 GB | 729 GB |
| **Attributed to `com.mcbench`** | **0.033%** | 0% |
| G1 young avg pause | 139 ms | 107 ms |

Capture accounts for **0.033%** of allocation, and what remains is on the writer
thread (`EventRing.drainTo` building RawEvents, `deflate` compressing frames).
The GC pause difference tracks player count (210 vs 171 connected), not the
plugin — a 0.03% allocation share cannot move young-gen pause times by 30%.

### Server-level A/B: no measurable difference

Four alternating runs at 250 players (all 250 connected, 0 failed), performance
power mode, on an already-warm world:

| Run | TPS | Median tick |
|---|---|---|
| without-1 | 17.52 | 56.1 ms |
| with-1 | 18.03 | 53.8 ms |
| without-2 | 18.16 | 53.4 ms |
| with-2 | 17.61 | 54.0 ms |
| **mean with plugin** | **17.82** | **53.9 ms** |
| **mean without** | **17.84** | **54.8 ms** |

The difference is **0.02 TPS**, and the plugin arm is nominally *faster* on median
tick time. The spread within the no-plugin arm alone (2.7 ms) is three times the
difference between arms. An effect whose sign flips between repeats and sits well
inside single-arm variance is not being measured — it is being estimated as zero.

Note what this does and does not establish. It is a **guardrail**, not a
measurement: the plugin costs ~0.05% of main-thread samples, while tick times
vary by several percent run to run, so this comparison could never resolve the
real figure. It rules out a gross regression. The profiler attribution is the
instrument that gives the actual number.

### Why the first A/B attempt was thrown out

An early with/without comparison appeared to show the plugin costing ~4 TPS, and
a later single pair appeared to show a 30% median-tick difference. Neither was
real. The first pair was not comparable at all — the no-plugin run happened
*second*, reusing terrain the first had generated, and connected only 171 players
against the other's 210. The second pair was a single sample per arm, and the
repeated runs above show the run-to-run spread is larger than the gap that
appeared.

The tell was internal inconsistency: a component responsible for 0.005–0.053% of
main-thread samples cannot cost 30% of tick time. When the cheap measurement and
the careful one disagree by three orders of magnitude, the cheap one is wrong.

Server TPS on this hardware is dominated by how much terrain is already generated
and how many players actually connected — neither of which is held constant by
simply running one config after the other. The matched comparison alternates arms
at a concurrency the server reliably reaches, and the profile attribution above is
the more robust evidence anyway, since it is a ratio within a single run.

## Production risk review

Beyond throughput, the failure modes that matter for a long-running server:

- **Silent data loss on quit — found and fixed.** `onQuit` removed the session
  from the very map the writer iterates, stranding up to one flush interval of
  events per departing player. A 250-player capture contained **250
  `session_start` markers and zero `session_end`**, and the loss was not even
  counted as dropped. Departed sessions now go to a hand-off queue that the
  writer drains once before discarding them. This is the kind of bug that makes a
  capture quietly wrong rather than obviously broken. Verified on a fresh
  250-player run: **250 `session_start` and 250 `session_end`**, against 250 / 0
  before the fix.
- **Unbounded growth — found and fixed.** A per-player login counter lived in a
  map that was never pruned, so it grew for as long as the plugin was loaded.
  Invisible across a benchmark run; a slow leak on a production server with
  player churn. The compiler only needs `(playerId, seq)` pairs to be distinct
  per session, so it is now a single monotonic counter with no per-player state.
- **A thrown flush cannot silently stop capture.** `scheduleWithFixedDelay`
  cancels all future executions if the task throws, so `WriterTask.run` catches
  `Throwable` deliberately. Without it, one transient IO error would disable
  capture for the rest of the run with no further warning.
- **Shutdown can stall the main thread briefly.** `onDisable` waits up to 5 s for
  an in-flight flush before its final drain. That is bounded and shutdown-only,
  but it is main-thread time.
- **Bounded memory.** Each session pre-allocates two rings — 8 KiB for movement
  packets, 2 KiB for main-thread events. At 1500 concurrent players that is
  **~15 MB**, allocated at join and never grown.
- **Two producers cannot share one ring — found on a live server and fixed.**
  A session is written by two threads: its Netty event loop for movement and the
  main thread for Bukkit events. They shared a single SPSC ring, whose producer
  binds to whichever thread writes first — always the main thread, recording
  `session_start` at join. Every movement packet for the rest of the session was
  then refused. A 329-player run captured **2,269 events and rejected 140,680**;
  nothing threw. Each producer now has its own ring and the writer drains both.
  Re-verified at 150 players: **zero refused, zero dropped**, and `move=965
  mob_spawn=3 marker=1` per session. `RingTest.twoProducersDoNotStarveEachOther`
  covers it.
- **Do not call `PacketEvents.setAPI()`/`init()`/`terminate()` when PacketEvents
  is a depend — found in review and fixed.** The PacketEvents *plugin* already
  does all four. Repeating them replaces the global API instance that every other
  PacketEvents plugin registered against, re-injects channel handlers, and — the
  severe one — `terminate()` on this plugin's disable would shut PacketEvents
  down **server-wide**, breaking the anticheat and shop plugins while the server
  kept running. This plugin now only registers and unregisters its own listener.
  That lifecycle dance is correct only when PacketEvents is shaded in.
- **Index leaks on a departing player — found in review and fixed.** `onQuit`
  evicted the player from the spatial index *before* retiring the session, so a
  movement packet still in flight would pass the session check and re-insert a
  departed player that nothing would ever remove again. Also, `PlayerIndex.update`
  used get-then-put, which lets the join thread and the Netty thread each build an
  entry; the loser stays in the cell map forever. Now `computeIfAbsent`, and the
  session is retired first.
- **Disk stalls cannot reach the main thread.** All IO is on the writer thread.
  If the disk fills or blocks, `WriterTask.run` catches the exception, keeps
  retrying, and the rings fill and drop events (counted). The server tick is
  unaffected.
- **No locks on the hot path.** The event handlers take no locks, do no IO, and
  allocate nothing. `flushOnce` is synchronised but runs on the writer thread;
  the one main-thread call is in `onDisable`, after the flush pool has been shut
  down and awaited.
- **Single-producer safety is enforced, not assumed.** Each ring binds its
  producer on first write and refuses anything else, counting it as `OFF-THREAD=`
  in the stats line rather than corrupting the ring. This is what surfaced the
  two-producer bug above, on a live server, as a number instead of as corrupt
  capture data — the single highest-value line of defensive code in the plugin.
  It confirms the Netty channel-pinning assumption every run: after the fix it is
  zero at 150 and 329 players.

## What is measured, and what is not

**Measured**: the plugin's own main-thread cost, in isolation, up to 3000
simulated players; correctness of the ring encoding end-to-end through the Go
reader; behaviour under a real 250-player movement load on Paper 26.1.2.

**Not measured**: a real 1500-player Paper server. That does not fit on one
16 GB machine — the server was already at 7 TPS with 41 players, saturated by
vanilla per-player work (chunk streaming alone moved 2.5 GB to the client in four
minutes) long before capture was a factor. The 1500-player figures above come
from the harness, which drives the real capture code at the event rate 1500
players would produce. That isolates the plugin's cost, which is the question
being asked, but it does not model contention with a genuinely loaded main
thread — real cache and memory pressure would make capture somewhat more
expensive than 0.82%.

To close that gap properly, run the harness numbers against a real server on
hardware that can host 1500 players, and compare `tps` with and without the
plugin loaded.

## Reproducing

```bash
cd capture-plugin && ./build.sh          # also runs RingTest
CP="libs/paper-api.jar;libs/annotations.jar;libs/adventure-api.jar;\
libs/adventure-key.jar;libs/examination-api.jar;libs/guava.jar"
javac --release 21 -cp "$CP" -d out-bench $(find src/main/java -name '*.java') tools/*.java

java -cp "out-bench;$CP" MainThreadBench 1500 30 20   # main-thread cost at 1500
java -cp "out-bench;$CP" ABBench 1500 400             # per-event variant A/B
java -cp "out-bench;$CP" MobAttrBench 1500 2000       # mob attribution, old vs new
java -cp "out-bench;$CP" RingTest                     # ring + index correctness
```

`tools/RingInteropFixture.java` writes a capture log through the real ring and
writer, which `bin/dump-capture` then decodes — the check that the packed slot
layout still produces the documented wire format.
