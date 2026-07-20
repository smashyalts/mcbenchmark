package com.mcbench.capture.model;

import java.lang.invoke.MethodHandles;
import java.lang.invoke.VarHandle;
import java.nio.ByteOrder;
import java.nio.charset.StandardCharsets;
import java.util.List;

/**
 * EventRing is a pre-allocated, allocation-free ring buffer of captured events
 * for one player.
 *
 * Why this exists: capture must hand events to a writer thread without doing
 * anything expensive at the point of capture. Building the wire encoding,
 * compressing and writing to disk all belong on the writer thread. A producer's
 * job here is to store primitives into memory that already exists and publish an
 * index. That matters most for Bukkit events, which cannot be received anywhere
 * but the main thread.
 *
 * <h3>Layout</h3>
 * One flat byte array, with each event packed into a {@value #SLOT_BYTES}-byte
 * slot — one cache line. This matters more than it looks: an earlier version
 * used parallel arrays (one per field), which was also allocation-free but
 * touched eight cache lines per event and measured <em>slower</em> than the
 * allocating design it replaced. Escape-analysed TLAB allocation is cheap and
 * cache-hot; a large cold buffer is not automatically better. Keeping a slot to
 * a single line is what makes the zero-allocation version actually win.
 *
 * <pre>
 *   0  tMicro      long
 *   8  epochMs     long
 *   16 dimensionId int
 *   20 coarseChunkX int
 *   24 coarseChunkZ int
 *   28 kind        int
 *   32 payloadLen  int
 *   36 payload     INLINE_PAYLOAD bytes
 * </pre>
 *
 * regionId is configured once per run and identical on every event, so it is
 * held on the ring rather than per slot. Payloads longer than the inline space
 * (only long command lines in practice) spill to a side array.
 *
 * <h3>Concurrency</h3>
 * Single-producer / single-consumer (the writer thread). The producer fills a
 * slot then publishes it with a release-store to {@code writeIdx}; the consumer
 * acquire-loads {@code writeIdx} before reading slot contents, so slot writes
 * cannot be observed out of order. {@link #claim} rejects a claim when the ring
 * is full, so a lagging writer costs dropped events (counted) rather than
 * unbounded memory.
 *
 * Which thread is the producer differs by ring — see {@link #claimProducer}.
 */
public final class EventRing {
    /** Inline payload bytes per slot; a movement payload is 21 B. */
    public static final int INLINE_PAYLOAD = 40;
    /** Fixed header bytes preceding the inline payload: three packed longs. */
    private static final int HEADER_BYTES = 24;
    /** One slot per cache line. */
    public static final int SLOT_BYTES = HEADER_BYTES + INLINE_PAYLOAD; // 64
    /**
     * Slots per ring. Sized for buffering, not for capacity: the writer flushes
     * every second and movement peaks near 20 events/sec/player, so 128 slots is
     * about six seconds of headroom. Bigger is actively worse — at 512 slots the
     * per-player buffers no longer fit in cache and the hot path measured 40%
     * slower than at 32.
     */
    private static final int DEFAULT_SLOTS = 128;

    /**
     * The thread allowed to produce into this ring, bound on first claim.
     *
     * The ring is single-producer, and that is now enforced per ring rather than
     * assumed. It has to be: a session has two producers — the connection's Netty
     * thread for movement packets and the main thread for Bukkit events — so the
     * binding lives here, on the thing that actually requires it, and each ring
     * gets exactly one. A second thread is refused and counted, which is how the
     * violation was caught on a live server rather than as silent corruption.
     */
    private volatile Thread producer;

    /** Binds or checks the producer thread. False means "refuse this event". */
    public boolean claimProducer(Thread t) {
        Thread p = producer;
        if (p == null) {
            producer = t;
            return true;
        }
        return p == t;
    }

    private static final VarHandle LONGS =
            MethodHandles.byteArrayViewVarHandle(long[].class, ByteOrder.nativeOrder());

    private final int slots;
    private final int mask;
    private final byte[] buf;         // slots * SLOT_BYTES
    private final byte[][] overflow;  // non-null only for payloads > INLINE_PAYLOAD
    private final String regionId;

    // The two index fields are written by different threads, so they are padded
    // apart: sharing a cache line would make every publish on the main thread
    // invalidate the writer thread's line and vice versa. The producer also keeps
    // a private cache of the consumer's index (readCache) and only re-reads the
    // real one when the ring looks full — otherwise the hot path would touch a
    // line the writer thread is actively modifying, which measured as the single
    // largest cost in this class.
    @SuppressWarnings("unused")
    private long pad00, pad01, pad02, pad03, pad04, pad05, pad06;
    private volatile long writeIdx;
    @SuppressWarnings("unused")
    private long pad10, pad11, pad12, pad13, pad14, pad15, pad16;
    private volatile long readIdx;
    @SuppressWarnings("unused")
    private long pad20, pad21, pad22, pad23, pad24, pad25, pad26;

    /** Producer-thread-only mirrors. Never read by the consumer. */
    private long writeLocal;
    private long readCache;
    private volatile long dropped;

    public EventRing(long maxBufferBytes, String regionId) {
        this(maxBufferBytes, regionId, DEFAULT_SLOTS);
    }

    public EventRing(long maxBufferBytes, String regionId, int maxSlots) {
        // The configured budget is a ceiling, not a target.
        int budget = (int) Math.max(32, maxBufferBytes / SLOT_BYTES);
        int wanted = Math.min(budget, maxSlots);
        this.slots = Integer.highestOneBit(wanted);
        this.mask = slots - 1;
        this.buf = new byte[slots * SLOT_BYTES];
        this.overflow = new byte[slots][];
        this.regionId = regionId == null ? "" : regionId;
    }

    public int slots() { return slots; }
    public long droppedEvents() { return dropped; }

    /**
     * Reserves the next slot, or returns -1 if the ring is full (writer lagging).
     * A successful claim must be followed by exactly one {@link #publish}.
     * Producer side only.
     *
     * The common case reads only producer-private fields — no shared memory is
     * touched until the ring actually looks full.
     */
    public long claim() {
        long w = writeLocal;
        if (w - readCache >= slots) {
            readCache = readIdx; // refresh only when we appear to be out of room
            if (w - readCache >= slots) {
                dropped++;
                return -1;
            }
        }
        return w;
    }

    /** Byte offset of a claimed slot's inline payload region. Producer side only. */
    public int payloadOffset(long seq) {
        return ((int) (seq & mask)) * SLOT_BYTES + HEADER_BYTES;
    }

    /** The backing array, for encoding a payload in place. Producer side only. */
    public byte[] buffer() { return buf; }

    /**
     * Writes a claimed slot's header. Combined with the payload store this is
     * the whole main-thread cost of capturing an event.
     */
    public void header(long seq, long tMicro, int dim,
                       int chunkX, int chunkZ, int kind, int payloadLen) {
        int o = ((int) (seq & mask)) * SLOT_BYTES;
        LONGS.set(buf, o, tMicro);
        LONGS.set(buf, o + 8, ((long) chunkZ << 32) | (chunkX & 0xFFFFFFFFL));
        LONGS.set(buf, o + 16, ((long) (dim & 0xFFFF) << 48)
                | ((long) (kind & 0xFFFF) << 32) | (payloadLen & 0xFFFFFFFFL));
    }

    /**
     * Copies a payload built elsewhere into a claimed slot, returning its length.
     * Used by the cold event kinds (commands, inventory, mobs), which fire a few
     * times per player per minute and are not worth specialising.
     */
    public int payload(long seq, byte[] src) {
        int i = (int) (seq & mask);
        if (src.length <= INLINE_PAYLOAD) {
            System.arraycopy(src, 0, buf, i * SLOT_BYTES + HEADER_BYTES, src.length);
            overflow[i] = null;
        } else {
            // Rare: a long command line. One allocation beats truncating.
            overflow[i] = src.clone();
        }
        return src.length;
    }

    /**
     * Publishes a filled slot. The release-store here pairs with the consumer's
     * acquire-load in {@link #drainTo}, making every preceding slot write visible
     * to the writer thread.
     */
    public void publish(long seq) {
        writeLocal = seq + 1;
        writeIdx = seq + 1; // release: makes the slot writes above visible
    }

    /**
     * Drains all published events into sink as RawEvents. Consumer side only —
     * this is where allocation is allowed, because it runs on the writer thread.
     */
    public void drainTo(List<RawEvent> sink, byte[] playerId, int sessionSeq, long startEpochMs) {
        long r = readIdx;
        long w = writeIdx; // acquire: pairs with publish()
        for (; r < w; r++) {
            int i = (int) (r & mask);
            int o = i * SLOT_BYTES;
            RawEvent e = new RawEvent();
            long t = (long) LONGS.get(buf, o);
            long chunks = (long) LONGS.get(buf, o + 8);
            long packed = (long) LONGS.get(buf, o + 16);
            e.tMicro = t;
            // Wall-clock is only used for frame time bounds, so it is derived
            // here rather than read from the clock on the main thread.
            e.epochMs = startEpochMs + t / 1000L;
            e.playerId = playerId;
            e.sessionSeq = sessionSeq;
            e.coarseChunkX = (int) chunks;
            e.coarseChunkZ = (int) (chunks >> 32);
            e.dimensionId = (int) ((packed >> 48) & 0xFFFF);
            e.kind = (int) ((packed >> 32) & 0xFFFF);
            e.regionId = regionId;
            int len = (int) packed;
            byte[] big = overflow[i];
            if (big != null) {
                e.payload = big;
                overflow[i] = null; // drop the reference so the slot holds nothing
            } else {
                byte[] p = new byte[len];
                System.arraycopy(buf, o + HEADER_BYTES, p, 0, len);
                e.payload = p;
            }
            sink.add(e);
        }
        readIdx = r;
    }

    /**
     * Encodes every published event straight into the frame buffer and returns
     * how many were written. Consumer side only.
     *
     * This is the production drain. {@link #drainTo} materialises a RawEvent and
     * a payload copy per event, which is convenient for tests and hopeless for
     * throughput: at 30,000 events/sec it generated enough garbage to trigger a
     * young GC every few seconds, and a young GC is stop-the-world — it pauses
     * the tick this plugin exists not to disturb.
     *
     * The per-event length prefix is computed arithmetically rather than by
     * encoding into a scratch buffer and measuring it, so nothing is copied
     * twice and nothing is allocated at all.
     *
     * @param regionIdUtf8 pre-encoded region id, since it is identical on every
     *                     event and re-encoding it per event allocated a byte[]
     * @param bounds       reused 2-element array the caller supplies; widened to
     *                     [min, max] tMicro so the frame header can carry the
     *                     batch's time span without a per-event object
     */
    public int encodeTo(com.mcbench.capture.io.ByteWriter sink, byte[] playerId,
                        int sessionSeq, byte[] regionIdUtf8, long[] bounds) {
        long r = readIdx;
        long w = writeIdx; // acquire: pairs with publish()
        int n = 0;
        for (; r < w; r++, n++) {
            int i = (int) (r & mask);
            int o = i * SLOT_BYTES;
            long t = (long) LONGS.get(buf, o);
            long chunks = (long) LONGS.get(buf, o + 8);
            long packed = (long) LONGS.get(buf, o + 16);
            int chunkX = (int) chunks;
            int chunkZ = (int) (chunks >> 32);
            int dim = (int) ((packed >> 48) & 0xFFFF);
            int kind = (int) ((packed >> 32) & 0xFFFF);
            int payloadLen = (int) packed;
            byte[] big = overflow[i];
            if (t < bounds[0]) {
                bounds[0] = t;
            }
            if (t > bounds[1]) {
                bounds[1] = t;
            }

            int body = 8 + 32
                    + com.mcbench.capture.io.ByteWriter.varIntSize(sessionSeq)
                    + com.mcbench.capture.io.ByteWriter.varIntSize(dim)
                    + com.mcbench.capture.io.ByteWriter.varIntSize(chunkX)
                    + com.mcbench.capture.io.ByteWriter.varIntSize(chunkZ)
                    + com.mcbench.capture.io.ByteWriter.varIntSize(regionIdUtf8.length)
                    + regionIdUtf8.length
                    + com.mcbench.capture.io.ByteWriter.varIntSize(kind)
                    + com.mcbench.capture.io.ByteWriter.varIntSize(payloadLen)
                    + payloadLen;

            sink.varInt(body);
            sink.int64BE(t);
            sink.raw(playerId);
            sink.varInt(sessionSeq);
            sink.varInt(dim);
            sink.varInt(chunkX);
            sink.varInt(chunkZ);
            sink.stringBytes(regionIdUtf8);
            sink.varInt(kind);
            sink.varInt(payloadLen);
            if (big != null) {
                sink.raw(big, 0, payloadLen);
                overflow[i] = null; // drop the reference so the slot holds nothing
            } else {
                sink.raw(buf, o + HEADER_BYTES, payloadLen);
            }
        }
        readIdx = r;
        return n;
    }

    /** Little-endian float32 store, matching the RawEvent payload encoding. */
    public static void putFloatLE(byte[] b, int off, float f) {
        int bits = Float.floatToRawIntBits(f);
        b[off] = (byte) bits;
        b[off + 1] = (byte) (bits >>> 8);
        b[off + 2] = (byte) (bits >>> 16);
        b[off + 3] = (byte) (bits >>> 24);
    }

    /** Minecraft VarInt store; returns the new offset. */
    public static int putVarInt(byte[] b, int off, int value) {
        int v = value;
        while (true) {
            if ((v & ~0x7F) == 0) {
                b[off++] = (byte) v;
                return off;
            }
            b[off++] = (byte) ((v & 0x7F) | 0x80);
            v >>>= 7;
        }
    }

    /** VarInt-prefixed UTF-8 string store; returns the new offset. */
    public static int putString(byte[] b, int off, String s) {
        byte[] sb = s.getBytes(StandardCharsets.UTF_8);
        off = putVarInt(b, off, sb.length);
        System.arraycopy(sb, 0, b, off, sb.length);
        return off + sb.length;
    }
}
