import com.mcbench.capture.model.EventRing;
import com.mcbench.capture.model.Payloads;
import com.mcbench.capture.model.RawEvent;

import org.bukkit.Location;

import java.util.Arrays;
import java.util.Map;
import java.util.Queue;
import java.util.UUID;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.ConcurrentLinkedQueue;

/**
 * ABBench compares the capture hot path variants inside one JVM, alternating
 * between them round by round so drift, thermal state and JIT noise land on both
 * equally. Comparing across separate JVM runs was giving 20% swings, which is
 * larger than the effects being measured.
 *
 * Variants:
 *   LEGACY  — allocate a payload array, a RawEvent and a queue node per event
 *   RING    — encode into a pre-allocated slot (the current design)
 *   LOOKUP  — the shared prefix only (session map lookup), to size the floor
 *   CLOCK   — lookup + System.nanoTime(), to price the timestamp
 *
 * The last two exist because the interesting question is not "which variant
 * wins" but "how much of the cost is even removable" — if the map lookup and
 * clock read dominate, optimising the encoding is pointless.
 *
 *   java -cp ... ABBench [players] [rounds]
 */
public final class ABBench {
    private static final int EVENTS_PER_ROUND_PER_PLAYER = 1;

    static final class LegacySession {
        final Queue<RawEvent> pending = new ConcurrentLinkedQueue<>();
    }

    public static void main(String[] args) {
        int players = args.length > 0 ? Integer.parseInt(args[0]) : 1500;
        int rounds = args.length > 1 ? Integer.parseInt(args[1]) : 400;
        long ringBytes = args.length > 2 ? Long.parseLong(args[2]) : 32L * 1024;

        UUID[] ids = new UUID[players];
        Location[] locs = new Location[players];
        Map<UUID, LegacySession> legacy = new ConcurrentHashMap<>();
        Map<UUID, EventRing> rings = new ConcurrentHashMap<>();
        for (int i = 0; i < players; i++) {
            ids[i] = new UUID(0x5EEDL, i);
            locs[i] = new Location(null, (i % 500) * 16, 64, (i / 500) * 16, 0f, 0f);
            legacy.put(ids[i], new LegacySession());
            rings.put(ids[i], new EventRing(ringBytes, ""));
        }

        long[] legacyNs = new long[rounds];
        long[] ringNs = new long[rounds];
        long[] lookupNs = new long[rounds];
        long[] clockNs = new long[rounds];

        for (int w = 0; w < 100; w++) {
            runLegacy(legacy, ids, locs, players);
            runRing(rings, ids, locs, players);
            runLookup(rings, ids, players);
            runClock(rings, ids, players);
            drainLegacy(legacy);
            drainRings(rings);
        }

        for (int r = 0; r < rounds; r++) {
            // Drain first so neither variant is measured against a full buffer.
            drainLegacy(legacy);
            drainRings(rings);

            long t0 = System.nanoTime();
            runLegacy(legacy, ids, locs, players);
            legacyNs[r] = System.nanoTime() - t0;
            drainLegacy(legacy);

            t0 = System.nanoTime();
            runRing(rings, ids, locs, players);
            ringNs[r] = System.nanoTime() - t0;
            drainRings(rings);

            t0 = System.nanoTime();
            runLookup(rings, ids, players);
            lookupNs[r] = System.nanoTime() - t0;

            t0 = System.nanoTime();
            runClock(rings, ids, players);
            clockNs[r] = System.nanoTime() - t0;
        }

        int n = players * EVENTS_PER_ROUND_PER_PLAYER;
        System.out.printf("players=%d rounds=%d ringBytes=%d slots=%d  (median ns/event)%n", players, rounds, ringBytes, rings.get(ids[0]).slots());
        report("LOOKUP  (map get only)      ", lookupNs, n);
        report("CLOCK   (lookup + nanoTime) ", clockNs, n);
        report("LEGACY  (alloc per event)   ", legacyNs, n);
        report("RING    (preallocated slot) ", ringNs, n);
    }

    private static void report(String label, long[] ns, int perRound) {
        long[] s = ns.clone();
        Arrays.sort(s);
        double med = s[s.length / 2] / (double) perRound;
        double p10 = s[s.length / 10] / (double) perRound;
        double p90 = s[s.length * 9 / 10] / (double) perRound;
        System.out.printf("%s %6.1f ns   (p10 %.1f, p90 %.1f)%n", label, med, p10, p90);
    }

    /** The old design: payload array + RawEvent + queue node per event. */
    private static void runLegacy(Map<UUID, LegacySession> map, UUID[] ids, Location[] locs, int players) {
        for (int i = 0; i < players; i++) {
            LegacySession s = map.get(ids[i]);
            if (s == null) continue;
            byte[] payload = Payloads.move(0.21f, 0f, 0.13f, 90f, 0f, true);
            RawEvent e = new RawEvent();
            e.tMicro = System.nanoTime() / 1000L;
            e.epochMs = System.currentTimeMillis();
            e.playerId = null;
            e.sessionSeq = 0;
            Location loc = locs[i];
            e.dimensionId = 0;
            e.coarseChunkX = Math.floorDiv(loc.getBlockX() >> 4, 4);
            e.coarseChunkZ = Math.floorDiv(loc.getBlockZ() >> 4, 4);
            e.regionId = "";
            e.kind = RawEvent.KIND_MOVE;
            e.payload = payload;
            s.pending.add(e);
        }
    }

    /** The current design: encode straight into a preallocated slot. */
    private static long tickMicros;

    private static void runRing(Map<UUID, EventRing> map, UUID[] ids, Location[] locs, int players) {
        tickMicros = System.nanoTime() / 1000L; // once per tick, as the plugin does
        for (int i = 0; i < players; i++) {
            EventRing ring = map.get(ids[i]);
            if (ring == null) continue;
            long seq = ring.claim();
            if (seq < 0) continue;
            byte[] buf = ring.buffer();
            int off = ring.payloadOffset(seq);
            EventRing.putFloatLE(buf, off, 0.21f);
            EventRing.putFloatLE(buf, off + 4, 0f);
            EventRing.putFloatLE(buf, off + 8, 0.13f);
            EventRing.putFloatLE(buf, off + 12, 90f);
            EventRing.putFloatLE(buf, off + 16, 0f);
            buf[off + 20] = 1;
            Location loc = locs[i];
            // Timestamp comes from the per-tick cache, not the clock.
            ring.header(seq, tickMicros, 0,
                    Math.floorDiv(loc.getBlockX() >> 4, 4),
                    Math.floorDiv(loc.getBlockZ() >> 4, 4),
                    RawEvent.KIND_MOVE, 21);
            ring.publish(seq);
        }
    }

    private static long sink;

    /** Floor: just the per-event session lookup every variant has to do. */
    private static void runLookup(Map<UUID, EventRing> map, UUID[] ids, int players) {
        long acc = 0;
        for (int i = 0; i < players; i++) {
            EventRing ring = map.get(ids[i]);
            if (ring != null) acc += ring.slots();
        }
        sink = acc;
    }

    /** Floor + the clock read each event needs for its timestamp. */
    private static void runClock(Map<UUID, EventRing> map, UUID[] ids, int players) {
        long acc = 0;
        for (int i = 0; i < players; i++) {
            EventRing ring = map.get(ids[i]);
            if (ring != null) acc += ring.slots() + System.nanoTime();
        }
        sink = acc;
    }

    private static void drainLegacy(Map<UUID, LegacySession> map) {
        for (LegacySession s : map.values()) {
            while (s.pending.poll() != null) { /* discard */ }
        }
    }

    private static void drainRings(Map<UUID, EventRing> map) {
        java.util.List<RawEvent> sink = new java.util.ArrayList<>();
        byte[] pid = new byte[32];
        for (EventRing r : map.values()) {
            sink.clear();
            r.drainTo(sink, pid, 0, 0L);
        }
    }
}
