import com.mcbench.capture.io.CaptureLogWriter;
import com.mcbench.capture.model.Payloads;
import com.mcbench.capture.model.RawEvent;

import java.nio.file.Path;
import java.security.MessageDigest;
import java.util.ArrayList;
import java.util.List;

/**
 * InteropFixture writes a capture log using the real plugin encoding classes,
 * so the Go reader can verify byte-for-byte compatibility. Not part of the
 * plugin jar; run manually during cross-language verification.
 *
 *   javac -d out src/main/java/com/mcbench/capture/io/*.java \
 *         src/main/java/com/mcbench/capture/model/*.java tools/InteropFixture.java
 *   java  -cp out InteropFixture <output-file>
 */
public final class InteropFixture {
    public static void main(String[] args) throws Exception {
        if (args.length < 1) {
            System.err.println("usage: InteropFixture <output raw-*.bin>");
            System.exit(2);
        }
        Path outFile = Path.of(args[0]);

        byte[] pid = sha256("player-uuid-0|salt");
        List<RawEvent> events = new ArrayList<>();

        events.add(ev(0, pid, RawEvent.KIND_MARKER, Payloads.marker("arena_start")));
        events.add(ev(1_000, pid, RawEvent.KIND_MOVE,
                Payloads.move(0.1f, 0.0f, -0.2f, 90.5f, 12.25f, true)));
        events.add(ev(2_000, pid, RawEvent.KIND_SPRINT_TOGGLE, Payloads.toggle(true)));
        events.add(ev(3_000, pid, RawEvent.KIND_DIG, Payloads.dig(2, 10, 64, -5, 1)));
        events.add(ev(4_000, pid, RawEvent.KIND_PLACE_BLOCK, Payloads.place(11, 64, -5, 1, 0)));
        events.add(ev(5_000, pid, RawEvent.KIND_ATTACK_ENTITY,
                Payloads.entityRef("minecraft:zombie", 0)));
        events.add(ev(6_000, pid, RawEvent.KIND_CMD, Payloads.command("/say hello world")));
        // Negative coarse chunk coords exercise 5-byte VarInt encoding.
        RawEvent neg = ev(7_000, pid, RawEvent.KIND_MOVE,
                Payloads.move(-1.5f, 0f, 1.5f, -180f, -45f, false));
        neg.coarseChunkX = -3;
        neg.coarseChunkZ = -7;
        neg.dimensionId = 0;
        events.add(neg);

        try (CaptureLogWriter w = new CaptureLogWriter(1, "interop-server")) {
            w.open(outFile);
            w.writeFrame(events, 100L, 200L);
            // Second frame to exercise multi-frame reading.
            List<RawEvent> more = new ArrayList<>();
            more.add(ev(8_000, pid, RawEvent.KIND_MARKER, Payloads.marker("round_end")));
            // Big-endian doubles/floats: the only place the format uses them,
            // and the two payloads that decide where a replay bot stands.
            more.add(ev(9_000, pid, RawEvent.KIND_REANCHOR,
                    Payloads.reanchor(1600.5, 72.0, -800.25, 90.5f, -12.25f, 1)));
            more.add(ev(10_000, pid, RawEvent.KIND_MARKER,
                    Payloads.markerAt("session_start", -804.5, 79.0, -52.25, 45.5f, 3.75f)));
            // Held item, armor slot and offhand: the three slot ranges that map
            // differently into player data.
            more.add(ev(11_000, pid, RawEvent.KIND_INVENTORY_SNAPSHOT,
                    Payloads.inventory(0,
                            new int[] { 0, 39, 40 },
                            new String[] { "minecraft:diamond_pickaxe",
                                           "minecraft:iron_helmet",
                                           "minecraft:shield" },
                            new int[] { 1, 1, 1 }, 3)));
            // The kinds captured from the wire rather than from Bukkit events.
            more.add(ev(12_000, pid, RawEvent.KIND_HELD_SLOT, Payloads.heldSlot(4)));
            more.add(ev(13_000, pid, RawEvent.KIND_CHAT,
                    Payloads.chat("selling 64 diamonds at spawn")));
            more.add(ev(14_000, pid, RawEvent.KIND_DROP_ITEM, Payloads.dropItem(true)));
            more.add(ev(15_000, pid, RawEvent.KIND_SWAP_HANDS, new byte[0]));
            // A dig start, which capture could not observe until it read packets.
            more.add(ev(16_000, pid, RawEvent.KIND_DIG, Payloads.dig(0, 10, 64, -5, 1)));
            // An off-hand arm swing.
            more.add(ev(17_000, pid, RawEvent.KIND_SWING, Payloads.swing(1)));
            // Releasing a held use (bow shot / finish eating): no payload.
            more.add(ev(18_000, pid, RawEvent.KIND_USE_ITEM_RELEASE, new byte[0]));
            w.writeFrame(more, 300L, 400L);
        }
        System.out.println("wrote " + outFile);
    }

    private static RawEvent ev(long tMicro, byte[] pid, int kind, byte[] payload) {
        RawEvent e = new RawEvent();
        e.tMicro = tMicro;
        e.playerId = pid;
        e.sessionSeq = 0;
        e.dimensionId = 0;
        e.coarseChunkX = 2;
        e.coarseChunkZ = 0;
        e.regionId = "arena";
        e.kind = kind;
        e.payload = payload;
        return e;
    }

    private static byte[] sha256(String s) throws Exception {
        return MessageDigest.getInstance("SHA-256").digest(s.getBytes("UTF-8"));
    }
}
