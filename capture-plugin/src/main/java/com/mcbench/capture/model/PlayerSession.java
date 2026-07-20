package com.mcbench.capture.model;

/**
 * PlayerSession holds one player's anonymized id, login sequence, and a
 * pre-allocated ring of pending events drained by the WriterTask.
 *
 * The player id and session sequence live here rather than in each event: they
 * are constant for the session's lifetime, so storing them per event would be
 * pure main-thread cost. The writer thread stamps them on during drain.
 */
public final class PlayerSession {
    public final byte[] playerId;   // 32-byte anonymized id
    public final int sessionSeq;
    /** Which writer shard drains this session. Fixed for the session's life. */
    public final int shard;

    /**
     * Two rings, because a session has two producer threads and an SPSC ring can
     * only have one.
     *
     * Movement arrives on the connection's Netty event loop; everything else
     * arrives on the main thread, because Bukkit will not dispatch an event
     * anywhere else. Sharing one ring between them was a real bug, not a
     * theoretical one: whichever thread wrote first bound the ring, and on a live
     * server that was always the main thread recording session_start at join, so
     * every subsequent movement packet was refused. A 329-player run captured
     * 2,269 events and rejected 140,680.
     *
     * The alternative — one MPSC ring with a CAS on claim — would put a contended
     * atomic on the hottest path in the plugin to serialise two producers that
     * never actually need ordering against each other. The writer merges both
     * rings into one batch, and the trace compiler sorts by timestamp per
     * session, so their relative order at capture time does not matter.
     */
    private final EventRing packetRing;
    private final EventRing mainRing;

    /**
     * Last position seen for this player, used to turn the absolute coordinates a
     * movement packet carries into the deltas the trace format stores.
     *
     * Volatile because the baseline is seeded on the main thread at join while
     * packets may already be arriving on the connection's Netty thread. After
     * that only the Netty thread writes it.
     */
    private volatile double lastX, lastY, lastZ;
    private volatile float lastYaw, lastPitch;
    private volatile boolean havePos;

    /**
     * The dimension this player is currently in.
     *
     * A movement packet carries only coordinates — the client does not restate
     * which world it is in — so the packet path cannot derive this and would
     * otherwise stamp every movement as the overworld, silently mislocating every
     * player in the nether or the end. It is therefore tracked on the session,
     * written by the main thread at join and on world change, and read by the
     * Netty thread on each movement.
     */
    private volatile int dimensionId;

    public int dimensionId() { return dimensionId; }

    public void setDimensionId(int id) { this.dimensionId = id; }

    public boolean havePos() { return havePos; }

    public void setPos(double x, double y, double z, float yaw, float pitch) {
        lastX = x; lastY = y; lastZ = z; lastYaw = yaw; lastPitch = pitch;
        havePos = true;
    }

    public double lastX() { return lastX; }
    public double lastY() { return lastY; }
    public double lastZ() { return lastZ; }
    public float lastYaw() { return lastYaw; }
    public float lastPitch() { return lastPitch; }

    /**
     * Slots for the main-thread ring. The kinds that reach it — commands,
     * inventory, digging, mob attribution — fire a few times per player per
     * minute against a one-second flush, so this is buffering for a burst, not
     * capacity. Sizing it like the movement ring would trade ~9 MiB at 1500
     * players for headroom nothing uses.
     */
    private static final int MAIN_RING_SLOTS = 32;

    public PlayerSession(byte[] playerId, int sessionSeq, long maxBufferBytes, String regionId,
                         int shard) {
        this.playerId = playerId;
        this.sessionSeq = sessionSeq;
        this.shard = shard;
        this.packetRing = new EventRing(maxBufferBytes, regionId);
        this.mainRing = new EventRing(maxBufferBytes, regionId, MAIN_RING_SLOTS);
    }

    /** Movement ring, produced by the connection's Netty event-loop thread. */
    public EventRing packetRing() { return packetRing; }

    /** Bukkit-event ring, produced by the server main thread. */
    public EventRing mainRing() { return mainRing; }

    /**
     * Drains both rings into sink. Called from the writer thread.
     *
     * The two rings interleave arbitrarily in the output. That is fine: the
     * batch already mixes sessions, and trace-compiler sorts each session's
     * events by timestamp before compiling.
     */
    public void drainTo(java.util.List<RawEvent> sink, long startEpochMs) {
        packetRing.drainTo(sink, playerId, sessionSeq, startEpochMs);
        mainRing.drainTo(sink, playerId, sessionSeq, startEpochMs);
    }

    /**
     * Allocation-free drain: encodes both rings straight into the frame buffer
     * and returns how many events were written. This is what production uses;
     * {@link #drainTo} exists for tests and fixtures.
     */
    public int encodeTo(com.mcbench.capture.io.ByteWriter sink, byte[] regionIdUtf8,
                        long[] bounds) {
        int n = packetRing.encodeTo(sink, playerId, sessionSeq, regionIdUtf8, bounds);
        n += mainRing.encodeTo(sink, playerId, sessionSeq, regionIdUtf8, bounds);
        return n;
    }

    public long droppedEvents() {
        return packetRing.droppedEvents() + mainRing.droppedEvents();
    }
}
