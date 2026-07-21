import com.mcbench.capture.model.EventRing;
import com.mcbench.capture.model.PlayerIndex;
import com.mcbench.capture.model.Payloads;
import com.mcbench.capture.model.PlayerSession;
import com.mcbench.capture.model.RawEvent;

import java.util.ArrayList;
import java.util.List;
import java.util.UUID;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.atomic.AtomicReference;

/**
 * RingTest exercises EventRing and PlayerIndex edge cases. Plain asserts and a
 * non-zero exit on failure, so it runs from build.sh without pulling in a test
 * framework (the plugin builds with javac alone).
 *
 *   java -cp out RingTest
 */
public final class RingTest {
    private static int failures;

    public static void main(String[] args) throws Exception {
        wrapsAroundPreservingOrder();
        dropsWhenFullAndRecoversAfterDrain();
        overflowPayloadSurvivesRoundTrip();
        overflowSlotIsClearedAfterDrain();
        producerConsumerHandoffIsOrdered();
        indexFindsNearestAcrossCellBoundary();
        indexUpdatesCellOnMove();
        indexRemoveEvicts();
        indexUpdateAfterRemoveDoesNotResurrect();
        indexConcurrentCellMovesLeaveNoStaleEntry();
        inPlaceMobEncodingMatchesPayloads();
        twoProducersDoNotStarveEachOther();
        encodeToMatchesDrainTo();

        if (failures > 0) {
            System.out.println(failures + " CHECK(S) FAILED");
            System.exit(1);
        }
        System.out.println("all ring/index checks passed");
    }

    /**
     * Regression test for a bug that reached a live server.
     *
     * A session has two producers — the main thread for Bukkit events, the
     * connection's Netty thread for movement packets — and they used to share one
     * ring. Whichever wrote first bound the producer, and in practice that was
     * always the main thread recording session_start at join, so every movement
     * packet afterwards was refused. A 329-player run captured 2,269 events and
     * rejected 140,680; nothing threw, and the capture was simply almost empty.
     *
     * The two rings must therefore bind independently, and a session must accept
     * both producers concurrently.
     */
    private static void twoProducersDoNotStarveEachOther() throws Exception {
        PlayerSession s = new PlayerSession(new byte[32], 1, 8192, "r", 0);

        // Main thread claims the Bukkit ring first, exactly as onJoin does.
        check(s.mainRing().claimProducer(Thread.currentThread()),
                "main thread should claim the main ring");

        AtomicReference<String> err = new AtomicReference<>();
        Thread netty = new Thread(() -> {
            if (!s.packetRing().claimProducer(Thread.currentThread())) {
                err.set("netty thread was refused the packet ring");
                return;
            }
            if (s.mainRing().claimProducer(Thread.currentThread())) {
                err.set("main ring accepted a second producer");
            }
        }, "netty-sim");
        netty.start();
        netty.join();
        check(err.get() == null, String.valueOf(err.get()));

        // And a session's drain must cover both rings, not just one.
        long a = s.mainRing().claim();
        s.mainRing().header(a, 10, 0, 0, 0, RawEvent.KIND_MARKER, 0);
        s.mainRing().publish(a);
        long b = s.packetRing().claim();
        s.packetRing().header(b, 20, 0, 0, 0, RawEvent.KIND_MOVE, 0);
        s.packetRing().publish(b);

        List<RawEvent> out = new ArrayList<>();
        s.drainTo(out, 0);
        check(out.size() == 2, "drainTo must cover both rings, got " + out.size());
    }

    /**
     * The allocation-free encode path must produce exactly the bytes the
     * RawEvent path produces.
     *
     * encodeTo() is what production writes and drainTo() is what the tests and
     * the Go interop fixture exercise, so a divergence between them would mean
     * the format is verified on a path nobody ships. Both are driven over the
     * same events here and compared byte for byte.
     */
    private static void encodeToMatchesDrainTo() {
        byte[] pid = new byte[32];
        for (int i = 0; i < pid.length; i++) {
            pid[i] = (byte) (i * 3);
        }
        byte[] region = "arena-7".getBytes(java.nio.charset.StandardCharsets.UTF_8);
        byte[] big = new byte[120];
        java.util.Arrays.fill(big, (byte) 7);

        // Same events into two identical rings.
        EventRing a = new EventRing(8192, "arena-7");
        EventRing b = new EventRing(8192, "arena-7");
        for (EventRing ring : new EventRing[] { a, b }) {
            long s1 = ring.claim();
            ring.header(s1, 123456789L, 2, -3, -7, RawEvent.KIND_MOVE, 0);
            ring.publish(s1);
            long s2 = ring.claim();
            int l2 = ring.payload(s2, big);           // overflow path
            ring.header(s2, 987654321L, 0, 1000, -1000, RawEvent.KIND_CMD, l2);
            ring.publish(s2);
            long s3 = ring.claim();
            int l3 = ring.payload(s3, new byte[] { 1, 2, 3 });
            ring.header(s3, 5L, 1, 0, 0, RawEvent.KIND_DIG, l3);
            ring.publish(s3);
        }

        // Reference bytes via RawEvent.
        List<RawEvent> evs = new ArrayList<>();
        a.drainTo(evs, pid, 42, 0L);
        com.mcbench.capture.io.ByteWriter want = new com.mcbench.capture.io.ByteWriter();
        com.mcbench.capture.io.ByteWriter one = new com.mcbench.capture.io.ByteWriter();
        for (RawEvent e : evs) {
            one.reset();
            e.encode(one);
            want.varInt(one.length());
            want.raw(one.array(), 0, one.length());
        }

        com.mcbench.capture.io.ByteWriter got = new com.mcbench.capture.io.ByteWriter();
        long[] bounds = { Long.MAX_VALUE, Long.MIN_VALUE };
        int n = b.encodeTo(got, pid, 42, region, bounds);

        check(n == evs.size(), "encodeTo wrote " + n + " events, drainTo produced " + evs.size());
        check(got.length() == want.length(),
                "encoded length " + got.length() + " != reference " + want.length());
        for (int i = 0; i < want.length(); i++) {
            if (got.array()[i] != want.array()[i]) {
                check(false, "encoded bytes differ at offset " + i);
                break;
            }
        }
        check(bounds[0] == 5L && bounds[1] == 987654321L,
                "time bounds wrong: " + bounds[0] + ".." + bounds[1]);
    }

    /** The ring must survive more events than it has slots without corruption. */
    private static void wrapsAroundPreservingOrder() {
        EventRing ring = new EventRing(4096, "r");
        int slots = ring.slots();
        List<RawEvent> out = new ArrayList<>();
        // Three full laps, draining each time, so sequence numbers wrap the mask.
        int n = 0;
        for (int lap = 0; lap < 3; lap++) {
            for (int i = 0; i < slots; i++) {
                long seq = ring.claim();
                check(seq >= 0, "claim should succeed after drain");
                ring.header(seq, n, 0, n, -n, RawEvent.KIND_MOVE, 0);
                ring.publish(seq);
                n++;
            }
            ring.drainTo(out, new byte[32], 0, 0L);
        }
        check(out.size() == slots * 3, "drained " + out.size() + " want " + slots * 3);
        for (int i = 0; i < out.size(); i++) {
            check(out.get(i).tMicro == i, "order/value broken at " + i);
            check(out.get(i).coarseChunkX == i, "chunkX broken at " + i);
            check(out.get(i).coarseChunkZ == -i, "negative chunkZ broken at " + i);
        }
    }

    /** Backpressure must drop and count, never overwrite unread slots. */
    private static void dropsWhenFullAndRecoversAfterDrain() {
        EventRing ring = new EventRing(4096, "r");
        int slots = ring.slots();
        for (int i = 0; i < slots; i++) {
            long seq = ring.claim();
            check(seq >= 0, "slot " + i + " should be claimable");
            ring.header(seq, i, 0, 0, 0, RawEvent.KIND_MOVE, 0);
            ring.publish(seq);
        }
        check(ring.claim() < 0, "ring should refuse when full");
        check(ring.droppedEvents() == 1, "drop should be counted, got " + ring.droppedEvents());

        List<RawEvent> out = new ArrayList<>();
        ring.drainTo(out, new byte[32], 0, 0L);
        check(out.size() == slots, "should drain a full ring");
        // The oldest entries must be intact — dropping protects unread data.
        check(out.get(0).tMicro == 0, "oldest event was overwritten");
        check(ring.claim() >= 0, "ring should accept again after drain");
    }

    /** Payloads larger than the inline slot take the overflow path. */
    private static void overflowPayloadSurvivesRoundTrip() {
        EventRing ring = new EventRing(4096, "r");
        byte[] big = new byte[200];
        for (int i = 0; i < big.length; i++) {
            big[i] = (byte) (i * 7);
        }
        byte[] small = { 1, 2, 3 };

        long s1 = ring.claim();
        int len1 = ring.payload(s1, big);
        ring.header(s1, 1, 0, 0, 0, RawEvent.KIND_CMD, len1);
        ring.publish(s1);

        long s2 = ring.claim();
        int len2 = ring.payload(s2, small);
        ring.header(s2, 2, 0, 0, 0, RawEvent.KIND_CMD, len2);
        ring.publish(s2);

        List<RawEvent> out = new ArrayList<>();
        ring.drainTo(out, new byte[32], 0, 0L);
        check(out.size() == 2, "expected 2 events");
        check(java.util.Arrays.equals(out.get(0).payload, big), "overflow payload corrupted");
        check(java.util.Arrays.equals(out.get(1).payload, small), "inline payload corrupted");
    }

    /**
     * A slot that once held an overflow payload must not hand that stale array
     * back when it is later reused for an inline one.
     */
    private static void overflowSlotIsClearedAfterDrain() {
        EventRing ring = new EventRing(4096, "r");
        int slots = ring.slots();
        byte[] big = new byte[200];
        java.util.Arrays.fill(big, (byte) 9);

        long seq = ring.claim();
        int len = ring.payload(seq, big);
        ring.header(seq, 1, 0, 0, 0, RawEvent.KIND_CMD, len);
        ring.publish(seq);
        List<RawEvent> out = new ArrayList<>();
        ring.drainTo(out, new byte[32], 0, 0L);

        // Come all the way round to the same slot with a small payload.
        byte[] small = { 42 };
        for (int i = 0; i < slots; i++) {
            long s = ring.claim();
            int l = ring.payload(s, small);
            ring.header(s, 100 + i, 0, 0, 0, RawEvent.KIND_CMD, l);
            ring.publish(s);
        }
        out.clear();
        ring.drainTo(out, new byte[32], 0, 0L);
        for (RawEvent e : out) {
            check(e.payload.length == 1 && e.payload[0] == 42,
                    "stale overflow payload resurfaced (len=" + e.payload.length + ")");
        }
    }

    /**
     * The publish/drain handoff must make slot contents visible to the consumer.
     * Not a proof of the memory model, but it catches a missing release-store.
     */
    private static void producerConsumerHandoffIsOrdered() throws Exception {
        final int total = 200_000;
        EventRing ring = new EventRing(4096, "r");
        CountDownLatch done = new CountDownLatch(1);
        AtomicReference<String> err = new AtomicReference<>();

        Thread consumer = new Thread(() -> {
            List<RawEvent> out = new ArrayList<>();
            long seen = 0;
            while (seen < total) {
                out.clear();
                ring.drainTo(out, new byte[32], 0, 0L);
                for (RawEvent e : out) {
                    // Payload was written before publish; it must match the header.
                    if (e.payload.length != 4 || readInt(e.payload) != (int) e.tMicro) {
                        err.compareAndSet(null, "torn event at tMicro=" + e.tMicro);
                        done.countDown();
                        return;
                    }
                    seen++;
                }
            }
            done.countDown();
        }, "consumer");
        consumer.setDaemon(true);
        consumer.start();

        for (int i = 0; i < total; i++) {
            long seq;
            while ((seq = ring.claim()) < 0) {
                Thread.onSpinWait(); // wait for the consumer instead of dropping
            }
            byte[] buf = ring.buffer();
            int off = ring.payloadOffset(seq);
            writeInt(buf, off, i);
            ring.header(seq, i, 0, 0, 0, RawEvent.KIND_MOVE, 4);
            ring.publish(seq);
        }
        done.await();
        check(err.get() == null, String.valueOf(err.get()));
    }

    private static void indexFindsNearestAcrossCellBoundary() {
        PlayerIndex idx = new PlayerIndex();
        UUID a = new UUID(1, 1);
        UUID b = new UUID(2, 2);
        // Deliberately either side of a 64-block cell edge.
        idx.add(a, 63, 64, 63);
        idx.add(b, 65, 64, 65);
        check(a.equals(idx.nearest(62, 64, 62)), "should find A");
        check(b.equals(idx.nearest(70, 64, 70)), "should find B across the boundary");
        // Beyond the 48-block attribution radius, nothing should match.
        check(idx.nearest(500, 64, 500) == null, "should not match far away");
    }

    private static void indexUpdatesCellOnMove() {
        PlayerIndex idx = new PlayerIndex();
        UUID a = new UUID(1, 1);
        idx.add(a, 10, 64, 10);
        check(a.equals(idx.nearest(12, 64, 12)), "should find A at origin");
        idx.update(a, 1000, 64, 1000); // crosses many cells
        check(idx.nearest(12, 64, 12) == null, "stale cell entry left behind");
        check(a.equals(idx.nearest(1002, 64, 1002)), "should find A at new cell");
        check(idx.size() == 1, "player counted twice, size=" + idx.size());
    }

    private static void indexRemoveEvicts() {
        PlayerIndex idx = new PlayerIndex();
        UUID a = new UUID(1, 1);
        idx.add(a, 10, 64, 10);
        idx.remove(a);
        check(idx.nearest(10, 64, 10) == null, "removed player still found");
        check(idx.size() == 0, "size should be 0 after remove");
    }

    /**
     * A movement packet still in flight when the player quits must not put them
     * back. The packet listener checks the session first, but that check and this
     * call are two steps with a quit possible in between; an update that created
     * entries would leave a player nothing ever removes again.
     */
    private static void indexUpdateAfterRemoveDoesNotResurrect() {
        PlayerIndex idx = new PlayerIndex();
        UUID a = new UUID(1, 1);
        idx.add(a, 10, 64, 10);
        idx.remove(a);
        idx.update(a, 12, 64, 12); // late packet
        check(idx.size() == 0, "late update resurrected a departed player, size=" + idx.size());
        check(idx.nearest(12, 64, 12) == null, "departed player is findable again");
    }

    /**
     * Two threads relocating the same player must not strand them in a cell the
     * entry no longer names — the main thread calls update on teleport, respawn
     * and world change while that player's Netty thread is capturing movement,
     * which is exactly when the cell changes.
     */
    private static void indexConcurrentCellMovesLeaveNoStaleEntry() throws Exception {
        PlayerIndex idx = new PlayerIndex();
        UUID a = new UUID(1, 1);
        idx.add(a, 0, 64, 0);
        final int rounds = 20000;
        Thread t = new Thread(() -> {
            for (int i = 0; i < rounds; i++) {
                idx.update(a, (i % 40) * 64, 64, 0); // walks across cells
            }
        });
        t.start();
        for (int i = 0; i < rounds; i++) {
            idx.update(a, 5000 + (i % 40) * 64, 64, 0); // "teleports" elsewhere
        }
        t.join();
        // One player is one entry in one cell. A stale copy cannot be found by
        // probing positions — it points at the same object, so it reports the
        // player's real current position — so count instead.
        check(idx.cellEntries() == 1,
                "player stranded in " + (idx.cellEntries() - 1) + " stale cell(s)");
        idx.remove(a);
        check(idx.size() == 0, "size should be 0 after remove");
        check(idx.cellEntries() == 0,
                "remove left " + idx.cellEntries() + " cell entry/entries behind");
    }

    /**
     * The in-place mob encoding must produce exactly what Payloads produced.
     *
     * CaptureManager.recordMobEvent writes the payload straight into a ring slot
     * to keep the main thread allocation-free under mob floods. That is a second
     * implementation of a wire format the Go decoder already reads, so it is
     * checked against the original rather than assumed equal.
     */
    private static void inPlaceMobEncodingMatchesPayloads() {
        byte[] buf = new byte[EventRing.INLINE_PAYLOAD];

        // Spawn: varInt(entityType), string(reason).
        int off = EventRing.putVarInt(buf, 0, 315);
        off = EventRing.putAscii(buf, off, "SPAWNER", EventRing.INLINE_PAYLOAD - off - 5);
        check(java.util.Arrays.equals(java.util.Arrays.copyOf(buf, off),
                        Payloads.mobSpawn(315, "SPAWNER")),
                "in-place mob_spawn encoding differs from Payloads.mobSpawn");

        // Despawn: varInt(entityType), varInt(reason).
        off = EventRing.putVarInt(buf, 0, 7);
        off = EventRing.putVarInt(buf, off, 0);
        check(java.util.Arrays.equals(java.util.Arrays.copyOf(buf, off),
                        Payloads.mobDespawn(7, 0)),
                "in-place mob_despawn encoding differs from Payloads.mobDespawn");

        // A tag longer than the slot allows is truncated, never written past the
        // slot into its neighbour.
        String huge = "X".repeat(200);
        off = EventRing.putVarInt(buf, 0, 1);
        off = EventRing.putAscii(buf, off, huge, EventRing.INLINE_PAYLOAD - off - 5);
        check(off <= EventRing.INLINE_PAYLOAD,
                "oversized tag ran past the slot: " + off + " > " + EventRing.INLINE_PAYLOAD);
    }

    private static int readInt(byte[] b) {
        return (b[0] & 0xFF) | ((b[1] & 0xFF) << 8) | ((b[2] & 0xFF) << 16) | ((b[3] & 0xFF) << 24);
    }

    private static void writeInt(byte[] b, int off, int v) {
        b[off] = (byte) v;
        b[off + 1] = (byte) (v >>> 8);
        b[off + 2] = (byte) (v >>> 16);
        b[off + 3] = (byte) (v >>> 24);
    }

    private static void check(boolean cond, String msg) {
        if (!cond) {
            System.out.println("FAIL: " + msg);
            failures++;
        }
    }
}
