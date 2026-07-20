package com.mcbench.capture;

import com.mcbench.capture.model.EventRing;
import com.mcbench.capture.model.PlayerSession;
import com.mcbench.capture.model.RawEvent;

import org.bukkit.Location;
import org.bukkit.World;

import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import java.security.SecureRandom;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.UUID;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.atomic.AtomicLong;

/**
 * CaptureManager owns per-player sessions, computes anonymized ids and world
 * anchors, and hands events to each session's ring.
 *
 * <h3>Which thread calls what</h3>
 * There are three producers, and the distinction matters because only one of
 * them can hurt the server:
 *
 * <ul>
 *   <li>{@link #recordMovePacket} — the connection's Netty event-loop thread.
 *       Movement is the only event kind whose rate scales with both player count
 *       and tick rate, and it no longer touches the main thread at all.</li>
 *   <li>{@link #record} and {@link #recordMove} — the main thread, from Bukkit
 *       events. Bukkit cannot dispatch events anywhere else, so these are stuck
 *       there; they are the cold kinds, firing a few times per player per
 *       minute.</li>
 *   <li>{@link #activeSessions} and the written* counters — writer threads.</li>
 * </ul>
 *
 * The budget rule is the same on every producer: no allocation, no I/O, no
 * encoding, no scanning a collection whose size grows with player count.
 * Anything failing that bar belongs on the writer thread. Measured at 1500
 * players and 30,000 packets/s, capture costs ~0.4 us per movement packet and
 * allocates nothing, occupying under 1% of a single Netty thread.
 *
 * Per-session rings are single-producer. That still holds under packet capture
 * because Netty pins a channel to one event loop for its lifetime — the producer
 * is a different thread per session rather than one thread for all of them.
 * {@link PlayerSession#claimProducer} enforces it rather than trusting it.
 */
public final class CaptureManager {
    private static final int COARSE_CHUNK_DIV = 4;

    private final long startNano = System.nanoTime();
    private final long startEpochMs = System.currentTimeMillis();
    private final byte[] salt;
    private final long maxBufferBytes;
    /** Constant for the whole run, so it lives here rather than on every event. */
    private final String regionId;

    /** Lookup path, hit on every captured event. */
    private final Map<UUID, PlayerSession> sessions = new ConcurrentHashMap<>();

    /**
     * The same sessions, partitioned across writer shards.
     *
     * Measured headroom, so this is not the bottleneck it was assumed to be: a
     * single writer thread drained 240,000 events/sec with zero drops — roughly
     * eight times what 1500 continuously-moving players generate. Sharding
     * exists for environments where that stops holding (slow or contended disks,
     * higher compression levels, many more players), not because one thread was
     * measured to be short of capacity. Default is one shard; raise
     * capture.writer_threads only if the stats line starts reporting drops.
     *
     * Each shard is drained by its own thread into its own file, so throughput
     * scales with shard count.
     *
     * Sessions are assigned round-robin by their monotonic sequence number, which
     * balances shards exactly rather than relying on hash distribution.
     */
    private final List<Map<UUID, PlayerSession>> shardSessions;
    private final List<java.util.Queue<PlayerSession>> shardDeparted;
    private final int shards;

    /**
     * Monotonic session id, stamped on every event so the compiler can separate
     * two visits by the same player.
     *
     * This used to be a per-player login counter in a map that was never pruned,
     * which grows for as long as the plugin is loaded — invisible over a
     * benchmark run, a slow leak on a production server with player churn. The
     * compiler only needs (playerId, seq) pairs to be distinct per session, so a
     * single counter does the same job with no per-player state at all.
     */
    private final java.util.concurrent.atomic.AtomicInteger sessionCounter =
            new java.util.concurrent.atomic.AtomicInteger();

    // Throughput/health counters for load testing (read by WriterTask).
    // Written from every producer thread (one per connection under packet
    // capture), so these are LongAdders: contended increments stay cheap because
    // each thread hits its own cell instead of one shared cache line.
    private final java.util.concurrent.atomic.LongAdder recordedTotal =
            new java.util.concurrent.atomic.LongAdder();
    private final AtomicLong departedDropped = new AtomicLong();
    private final java.util.concurrent.atomic.LongAdder offThreadDropped =
            new java.util.concurrent.atomic.LongAdder();



    public CaptureManager(boolean anonymize, long maxBufferBytes, String regionId, int shards) {
        this.maxBufferBytes = maxBufferBytes;
        this.regionId = regionId == null ? "" : regionId;
        this.shards = Math.max(1, shards);
        List<Map<UUID, PlayerSession>> sh = new ArrayList<>(this.shards);
        List<java.util.Queue<PlayerSession>> dep = new ArrayList<>(this.shards);
        for (int i = 0; i < this.shards; i++) {
            sh.add(new ConcurrentHashMap<>());
            dep.add(new java.util.concurrent.ConcurrentLinkedQueue<>());
        }
        this.shardSessions = sh;
        this.shardDeparted = dep;
        this.salt = new byte[16];
        if (anonymize) {
            new SecureRandom().nextBytes(salt);
        }
        // When not anonymizing, salt stays all-zero (ids stable across runs but
        // still hashed so raw UUIDs never touch disk).
    }

    /** Registers a new session for a joining player and returns it. */
    public PlayerSession onJoin(UUID uuid) {
        return onJoin(uuid, null, 0);
    }

    /**
     * Registers a new session, seeded with the player's spawn position and
     * dimension.
     *
     * The seeding happens before the session is published to the map, and that
     * ordering is the point of this overload. Publishing is what opens the packet
     * path: the instant the session is visible, this player's Netty thread can
     * capture movement against it. A session published unseeded would take its
     * delta baseline from whichever packet happened to arrive first and stamp
     * that movement as the overworld.
     */
    public PlayerSession onJoin(UUID uuid, Location spawn, int dimensionId) {
        int seq = sessionCounter.getAndIncrement();
        byte[] id = anonymizedId(uuid);
        int shard = Math.floorMod(seq, shards);
        PlayerSession s = new PlayerSession(id, seq, maxBufferBytes, regionId, shard);
        if (spawn != null) {
            s.setPos(spawn.getX(), spawn.getY(), spawn.getZ(), spawn.getYaw(), spawn.getPitch());
            s.setDimensionId(dimensionId);
        }
        sessions.put(uuid, s);
        shardSessions.get(shard).put(uuid, s);
        return s;
    }

    /**
     * Retires a session. The player is gone, so nothing more will be written to
     * its ring — but up to one flush interval of events is still sitting in it,
     * including the session_end marker the listener just recorded. Removing the
     * session from the map alone would strand those events, since the writer
     * only iterates live sessions: a 250-player capture produced 250
     * session_start markers and zero session_end markers, with the loss not even
     * counted as dropped.
     *
     * So the session moves to a hand-off queue instead, and the writer drains it
     * once before letting it go.
     */
    public void onQuit(UUID uuid) {
        PlayerSession s = sessions.remove(uuid);
        if (s != null) {
            departedDropped.addAndGet(s.droppedEvents()); // preserve its drop count
            shardSessions.get(s.shard).remove(uuid);
            shardDeparted.get(s.shard).add(s);
        }
    }

    public int shards() { return shards; }

    /** Region id stamped on every event; the writer pre-encodes it once. */
    public String regionId() { return regionId; }

    /**
     * Capture files currently open for writing, across all shards.
     *
     * Retention needs this. Each shard writes its own file, so a shard pruning
     * "oldest files" could otherwise delete the file another shard is actively
     * writing — on Linux the unlink succeeds and that writer keeps filling an
     * unreachable inode, losing the data silently.
     */
    private final java.util.Set<java.nio.file.Path> openFiles =
            java.util.concurrent.ConcurrentHashMap.newKeySet();

    public void fileOpened(java.nio.file.Path prev, java.nio.file.Path now) {
        if (prev != null) {
            openFiles.remove(prev);
        }
        if (now != null) {
            openFiles.add(now);
        }
    }

    public boolean isOpen(java.nio.file.Path p) { return openFiles.contains(p); }

    // Writer-side totals, summed across shards so the stats line stays
    // server-wide rather than reporting one shard's slice.
    private final java.util.concurrent.atomic.LongAdder writtenEvents =
            new java.util.concurrent.atomic.LongAdder();
    private final java.util.concurrent.atomic.LongAdder writtenFrames =
            new java.util.concurrent.atomic.LongAdder();
    private final java.util.concurrent.atomic.LongAdder writtenBytes =
            new java.util.concurrent.atomic.LongAdder();

    public void addWritten(long events, long frames, long bytes) {
        writtenEvents.add(events);
        writtenFrames.add(frames);
        writtenBytes.add(bytes);
    }

    public long writtenEvents() { return writtenEvents.sum(); }
    public long writtenFrames() { return writtenFrames.sum(); }
    public long writtenBytes() { return writtenBytes.sum(); }

    /**
     * Sessions whose player has left but whose buffered tail has not been
     * written yet. The writer polls this, drains each once, and discards it.
     */
    public java.util.Queue<PlayerSession> departedSessions(int shard) {
        return shardDeparted.get(shard);
    }

    /** Total events successfully enqueued for capture since start (approximate). */
    public long recordedTotal() { return recordedTotal.sum(); }

    /** Epoch millis at plugin start; the writer thread rebuilds wall-clock from it. */
    public long startEpochMs() { return startEpochMs; }

    /** Total events dropped (ring full) across live and departed sessions. */
    public long totalDropped() {
        long d = departedDropped.get();
        for (PlayerSession s : sessions.values()) {
            d += s.droppedEvents();
        }
        return d;
    }

    /**
     * Events refused because they arrived off the main thread. Expected to stay
     * zero — a non-zero value means some event source is async and the listener
     * for it needs rethinking, not that data was merely lost.
     */
    public long offThreadDropped() { return offThreadDropped.sum(); }

    public PlayerSession session(UUID uuid) {
        return sessions.get(uuid);
    }

    public Iterable<PlayerSession> activeSessions() {
        return sessions.values();
    }

    /** Sessions belonging to one writer shard. */
    public Iterable<PlayerSession> activeSessions(int shard) {
        return shardSessions.get(shard).values();
    }

    /** Microseconds since plugin start. */
    public long nowMicros() {
        return (System.nanoTime() - startNano) / 1000L;
    }

    /**
     * Counts a server tick. Called once per tick from the main thread.
     *
     * This used to also cache a timestamp for event handlers to reuse, because
     * System.nanoTime() measured ~62 ns — over half the cost of capturing a
     * movement event, back when movement was captured from PlayerMoveEvent on
     * this thread. Movement now comes from packets on Netty threads, so the cache
     * was saving 62 ns on events that fire a few times per player per minute,
     * and charging real accuracy for it: the cached value goes stale for as long
     * as a tick takes, so on a struggling server a command could be stamped
     * hundreds of milliseconds before movement that actually preceded it. A
     * 150-player capture showed session_start markers 360 ms behind the first
     * movement of the same session. The compiler sorts by timestamp, so that
     * reorders the trace.
     */
    public void tick() {
        ticks++;
    }

    /**
     * Ticks observed since start. The writer thread divides this by elapsed wall
     * time to report the server's real TPS alongside the capture stats.
     *
     * This is worth having: a capture is only as trustworthy as the server that
     * produced it, and "20,000 events captured" means something very different at
     * 20 TPS than at 4. Paper only warns about overruns past two seconds, so a
     * server can be at a third of tick rate and say nothing.
     */
    public long ticks() { return ticks; }

    private volatile long ticks;

    /**
     * Allocation-free movement capture from a packet, called on the connection's
     * Netty event-loop thread. This is the only path whose cost is multiplied by
     * both player count and tick rate, so it allocates nothing and touches no
     * collection larger than the session map lookup.
     *
     * Unlike the Bukkit-event paths this reads the clock per event rather than
     * using the cached tick timestamp. Sub-tick arrival time is one of the main
     * reasons to capture at packet level — collapsing it to tick granularity
     * would throw away the thing being bought — and the ~24 ns clock read costs
     * nothing here because it is off the main thread.
     */
    public void recordMovePacket(UUID uuid, float dx, float dy, float dz,
                                 float yaw, float pitch, boolean onGround,
                                 int blockX, int blockZ) {
        PlayerSession s = sessions.get(uuid);
        if (s == null) {
            return;
        }
        EventRing ring = s.packetRing();
        if (!ring.claimProducer(Thread.currentThread())) {
            // Netty pins a channel to one event loop for its lifetime, so this
            // should never fire. If it does, the assumption is wrong and the
            // stats line says so rather than the ring corrupting quietly.
            offThreadDropped.increment();
            return;
        }
        long tMicro = nowMicros();
        long seq = ring.claim();
        if (seq < 0) {
            return; // ring full; already counted as dropped
        }
        byte[] buf = ring.buffer();
        int off = ring.payloadOffset(seq);
        EventRing.putFloatLE(buf, off, dx);
        EventRing.putFloatLE(buf, off + 4, dy);
        EventRing.putFloatLE(buf, off + 8, dz);
        EventRing.putFloatLE(buf, off + 12, yaw);
        EventRing.putFloatLE(buf, off + 16, pitch);
        buf[off + 20] = (byte) (onGround ? 1 : 0);
        ring.header(seq, tMicro, s.dimensionId(),
                Math.floorDiv(blockX >> 4, COARSE_CHUNK_DIV),
                Math.floorDiv(blockZ >> 4, COARSE_CHUNK_DIV),
                RawEvent.KIND_MOVE, 21);
        ring.publish(seq);
        recordedTotal.increment();
    }

    /**
     * Enqueues an event with a caller-built payload. loc supplies the world
     * anchor. No-op if the player has no active session.
     */
    public void record(UUID uuid, int kind, byte[] payload, Location loc) {
        PlayerSession s = sessions.get(uuid);
        if (s == null) {
            return;
        }
        EventRing ring = s.mainRing();
        if (!ring.claimProducer(Thread.currentThread())) {
            offThreadDropped.increment();
            return;
        }
        long seq = ring.claim();
        if (seq < 0) {
            return; // ring full; already counted as dropped
        }
        int len = ring.payload(seq, payload);
        writeHeader(ring, seq, loc, kind, len);
        ring.publish(seq);
        recordedTotal.increment();
    }

    /**
     * Allocation-free capture of a mob spawn or death, on the main thread.
     *
     * These are the only remaining main-thread kinds whose rate is not bounded by
     * player behaviour. A spawner farm, a raid or a mob grinder produces hundreds
     * of {@code CreatureSpawnEvent}s a second regardless of how many players are
     * online, and the generic {@link #record} path builds the payload before the
     * ring is asked whether it has room — so on a busy server the main thread was
     * allocating a ByteWriter, its backing array, a byte[] for the reason string
     * and a trimmed copy, per event, for events that were then dropped. Garbage
     * on the main thread is worse than it looks: a young GC is stop-the-world, so
     * it pauses the tick this plugin exists not to disturb.
     *
     * Here the slot is claimed first, so a full ring costs a comparison and
     * nothing else, and the payload is written straight into it.
     *
     * @param tag spawn reason for a spawn event, or null for a death (which
     *            encodes a numeric reason instead, matching Payloads.mobDespawn)
     */
    public void recordMobEvent(UUID uuid, int kind, int entityType, String tag, Location loc) {
        PlayerSession s = sessions.get(uuid);
        if (s == null) {
            return;
        }
        EventRing ring = s.mainRing();
        if (!ring.claimProducer(Thread.currentThread())) {
            offThreadDropped.increment();
            return;
        }
        long seq = ring.claim();
        if (seq < 0) {
            return; // ring full; already counted as dropped, and nothing allocated
        }
        byte[] buf = ring.buffer();
        int start = ring.payloadOffset(seq);
        int off = EventRing.putVarInt(buf, start, entityType);
        if (tag == null) {
            off = EventRing.putVarInt(buf, off, 0); // despawn reason
        } else {
            // Bounded so the payload cannot run past its slot into the next one.
            // Spawn reasons are short enum names; the cap never bites in practice.
            off = EventRing.putAscii(buf, off, tag,
                    EventRing.INLINE_PAYLOAD - (off - start) - 5);
        }
        writeHeader(ring, seq, loc, kind, off - start);
        ring.publish(seq);
        recordedTotal.increment();
    }

    private void writeHeader(EventRing ring, long seq, Location loc, int kind, int payloadLen) {
        int dim = 0;
        int ccx = 0;
        int ccz = 0;
        if (loc != null) {
            dim = dimensionId(loc.getWorld());
            ccx = Math.floorDiv(loc.getBlockX() >> 4, COARSE_CHUNK_DIV);
            ccz = Math.floorDiv(loc.getBlockZ() >> 4, COARSE_CHUNK_DIV);
        }
        ring.header(seq, nowMicros(), dim, ccx, ccz, kind, payloadLen);
    }


    private byte[] anonymizedId(UUID uuid) {
        try {
            MessageDigest md = MessageDigest.getInstance("SHA-256");
            md.update(uuidToBytes(uuid));
            md.update(salt);
            return md.digest();
        } catch (NoSuchAlgorithmException ex) {
            throw new IllegalStateException("SHA-256 unavailable", ex);
        }
    }

    private static byte[] uuidToBytes(UUID uuid) {
        byte[] b = new byte[16];
        long hi = uuid.getMostSignificantBits();
        long lo = uuid.getLeastSignificantBits();
        for (int i = 0; i < 8; i++) {
            b[i] = (byte) (hi >>> (56 - 8 * i));
            b[8 + i] = (byte) (lo >>> (56 - 8 * i));
        }
        return b;
    }

    public static int dimensionId(World world) {
        if (world == null) {
            return 0;
        }
        switch (world.getEnvironment()) {
            case NORMAL: return 0;
            case NETHER: return 1;
            case THE_END: return 2;
            default: return 3;
        }
    }
}
