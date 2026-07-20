import com.mcbench.capture.CaptureManager;
import com.mcbench.capture.WriterTask;
import com.mcbench.capture.io.CaptureLogWriter;
import com.mcbench.capture.model.Payloads;
import com.mcbench.capture.model.RawEvent;

import org.bukkit.Location;

import java.nio.file.Path;
import java.util.UUID;
import java.util.logging.Logger;

/**
 * RingInteropFixture drives events through the real EventRing and WriterTask, so
 * the Go reader can verify the ring's packed slot layout still decodes to the
 * documented RawEvent wire format.
 *
 * InteropFixture builds RawEvents directly and so proves only that the encoder
 * matches Go. It would keep passing even if the ring corrupted every field on
 * the way in. This fixture covers the path production actually uses: main-thread
 * record -> packed slot -> writer-thread drain -> frame.
 *
 * The values below are deliberately awkward — negative coordinates (5-byte
 * VarInts), a payload longer than the inline slot space (forcing the overflow
 * path), and boundary floats — because those are where a packed layout breaks.
 *
 *   java -cp out RingInteropFixture <output-dir>
 */
public final class RingInteropFixture {
    public static void main(String[] args) throws Exception {
        if (args.length < 1) {
            System.err.println("usage: RingInteropFixture <output-dir>");
            System.exit(2);
        }
        Path outDir = Path.of(args[0]);

        CaptureManager mgr = new CaptureManager(false, 32L * 1024, "arena-7", 1);
        CaptureLogWriter writer = new CaptureLogWriter(1, "ring-interop");
        WriterTask task = new WriterTask(mgr, writer, outDir, 0, 0, Logger.getLogger("fixture"));

        UUID p = new UUID(0xABCDL, 1);
        mgr.onJoin(p);
        mgr.tick();

        Location pos = new Location(null, 100.5, 64, -200.5, 0f, 0f);
        // Negative coords exercise the 5-byte VarInt path on the coarse anchor.
        Location neg = new Location(null, -1600.25, 30, -1600.75, 0f, 0f);

        mgr.record(p, RawEvent.KIND_MARKER, Payloads.marker("session_start"), pos);
        mgr.recordMovePacket(p, 0.1f, 0.0f, -0.2f, 90.5f, 12.25f, true, 100, -200);
        mgr.recordMovePacket(p, -1.5f, 0f, 1.5f, -180f, -45f, false, -1601, -1601);
        mgr.record(p, RawEvent.KIND_SPRINT_TOGGLE, Payloads.toggle(true), pos);
        mgr.record(p, RawEvent.KIND_DIG, Payloads.dig(2, 10, 64, -5, 1), pos);
        mgr.record(p, RawEvent.KIND_PLACE_BLOCK, Payloads.place(11, 64, -5, 1, 0), pos);
        mgr.record(p, RawEvent.KIND_ATTACK_ENTITY, Payloads.attackEntity(3), pos);
        mgr.record(p, RawEvent.KIND_INV_OPEN, Payloads.invOpen(3, 12, 65, -7), pos);
        mgr.record(p, RawEvent.KIND_INV_CLICK, Payloads.invClick(0, 20, 0, 0), pos);
        mgr.record(p, RawEvent.KIND_CMD, Payloads.command("/say hello world"), pos);
        // Longer than INLINE_PAYLOAD (40 B), so this takes the overflow path.
        mgr.record(p, RawEvent.KIND_CMD,
                Payloads.command("/ah sell 12345 this is a deliberately long command line "
                        + "that cannot fit in the inline slot payload"), pos);
        mgr.record(p, RawEvent.KIND_MARKER, Payloads.marker("session_end"), pos);

        task.flushOnce();
        writer.close();
        System.out.println("wrote " + writer.currentPath());
        System.out.println("events recorded: " + mgr.recordedTotal()
                + " dropped: " + mgr.totalDropped()
                + " offThread: " + mgr.offThreadDropped());
    }
}
