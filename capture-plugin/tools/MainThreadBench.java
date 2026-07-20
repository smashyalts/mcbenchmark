import com.mcbench.capture.CaptureManager;
import com.mcbench.capture.WriterTask;
import com.mcbench.capture.io.CaptureLogWriter;
import com.mcbench.capture.model.Payloads;
import com.mcbench.capture.model.RawEvent;

import org.bukkit.Location;

import java.lang.management.ManagementFactory;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.Arrays;
import java.util.UUID;
import java.util.concurrent.Executors;
import java.util.concurrent.ScheduledExecutorService;
import java.util.concurrent.TimeUnit;
import java.util.logging.Logger;

/**
 * MainThreadBench measures what BenchCapture costs the Paper *main thread* at a
 * given player count and movement rate, without needing a real server.
 *
 * The question it answers: a Paper tick has a 50 ms budget. How much of it does
 * capture eat when N players are all moving? Bukkit dispatches PlayerMoveEvent on
 * the main thread, so this cost is unavoidable overhead on the server's critical
 * path — it must be a rounding error, not a slice.
 *
 * It drives the exact main-thread code path the movement listener runs
 * (payload build + CaptureManager.record) from one thread, tick-shaped: each
 * round produces players*movesPerTick events and is timed on its own. The real
 * WriterTask drains concurrently on a scheduler thread, so queue contention and
 * backpressure are real, not modelled.
 *
 *   java -cp ... MainThreadBench [players] [seconds] [movesPerSecPerPlayer]
 */
public final class MainThreadBench {
    private static final double TICK_MS = 50.0;

    public static void main(String[] args) throws Exception {
        int players = args.length > 0 ? Integer.parseInt(args[0]) : 1500;
        int seconds = args.length > 1 ? Integer.parseInt(args[1]) : 30;
        int movesPerSec = args.length > 2 ? Integer.parseInt(args[2]) : 20;
        int writerThreads = args.length > 3 ? Integer.parseInt(args[3]) : 1;

        int ticks = seconds * 20;
        int movesPerTick = Math.max(1, movesPerSec / 20);
        int eventsPerTick = players * movesPerTick;

        Path dir = Files.createTempDirectory("benchcapture-bench");
        Logger log = Logger.getLogger("bench");

        CaptureManager mgr = new CaptureManager(true, 32L * 1024, "", writerThreads);
        UUID[] ids = new UUID[players];
        Location[] locs = new Location[players];
        for (int i = 0; i < players; i++) {
            ids[i] = new UUID(0x5EEDL, i);
            mgr.onJoin(ids[i]);
            locs[i] = new Location(null, (i % 500) * 16, 64, (i / 500) * 16, 0f, 0f);
        }

        java.util.List<CaptureLogWriter> writerList = new java.util.ArrayList<>();
        java.util.List<WriterTask> taskList = new java.util.ArrayList<>();
        for (int i = 0; i < writerThreads; i++) {
            CaptureLogWriter w = new CaptureLogWriter(1, "bench");
            writerList.add(w);
            taskList.add(new WriterTask(mgr, w, dir, 0, 0, log, i));
        }
        ScheduledExecutorService sched = Executors.newScheduledThreadPool(writerThreads, r -> {
            Thread t = new Thread(r, "capture-writer");
            t.setDaemon(true);
            return t;
        });
        // Matches config default flush_interval_ticks: 20 (= 1 s).
        for (WriterTask t : taskList) {
            sched.scheduleAtFixedRate(t, 200, 1000, TimeUnit.MILLISECONDS);
        }

        var tmx = (com.sun.management.ThreadMXBean) ManagementFactory.getThreadMXBean();
        long self = Thread.currentThread().threadId();

        // Warm up so we time steady-state JIT'd code, not the interpreter.
        for (int t = 0; t < 200; t++) {
            produceTick(mgr, ids, locs, players, movesPerTick);
        }

        // Warmup runs unpaced, so it overruns the rings and drops events that say
        // nothing about steady state. Let the writer catch up, then count drops
        // only from here.
        Thread.sleep(1500);
        long dropBefore = mgr.totalDropped();

        long[] tickNanos = new long[ticks];
        long allocBefore = tmx.getThreadAllocatedBytes(self);
        long wallStart = System.nanoTime();
        for (int t = 0; t < ticks; t++) {
            long t0 = System.nanoTime();
            produceTick(mgr, ids, locs, players, movesPerTick);
            tickNanos[t] = System.nanoTime() - t0;
            // Sleep out the rest of the tick so the writer drains at a realistic
            // rate relative to production (else we'd measure an unbounded burst).
            long spentMs = tickNanos[t] / 1_000_000L;
            long restMs = 50 - spentMs;
            if (restMs > 0) {
                Thread.sleep(restMs);
            }
        }
        long wallNanos = System.nanoTime() - wallStart;
        long allocBytes = tmx.getThreadAllocatedBytes(self) - allocBefore;

        sched.shutdown();
        sched.awaitTermination(5, TimeUnit.SECONDS);
        for (WriterTask t : taskList) {
            t.flushOnce();
        }
        for (CaptureLogWriter w : writerList) {
            w.close();
        }

        long produced = (long) ticks * eventsPerTick;
        long dropped = mgr.totalDropped() - dropBefore;

        long[] sorted = tickNanos.clone();
        Arrays.sort(sorted);
        double meanMs = Arrays.stream(tickNanos).average().orElse(0) / 1e6;
        double p50 = sorted[sorted.length / 2] / 1e6;
        double p99 = sorted[(int) (sorted.length * 0.99)] / 1e6;
        double max = sorted[sorted.length - 1] / 1e6;
        double nsPerEvent = (double) Arrays.stream(tickNanos).sum() / produced;

        System.out.println("=== BenchCapture main-thread cost ===");
        System.out.printf("players=%d  moves/s/player=%d  ticks=%d  events/tick=%,d  writers=%d%n",
                players, movesPerSec, ticks, eventsPerTick, writerThreads);
        System.out.printf("produced=%,d  dropped=%,d (%.2f%%)  wall=%.1fs%n",
                produced, dropped, 100.0 * dropped / produced, wallNanos / 1e9);
        System.out.printf("per-event: %.0f ns   alloc: %.0f B/event  (%.1f MiB/s)%n",
                nsPerEvent, (double) allocBytes / produced,
                allocBytes / 1048576.0 / (wallNanos / 1e9));
        System.out.printf("per-tick main-thread: mean %.3f ms  p50 %.3f  p99 %.3f  max %.3f%n",
                meanMs, p50, p99, max);
        System.out.printf(">>> %.2f%% of the 50 ms tick budget (mean), %.2f%% (p99)%n",
                100 * meanMs / TICK_MS, 100 * p99 / TICK_MS);
        System.out.println("capture log dir: " + dir);
    }

    /**
     * One tick's worth of movement capture. This mirrors CaptureListener.onMove
     * exactly — payload build then record — so the measured cost is the real
     * main-thread path, not an approximation of it.
     */
    private static void produceTick(CaptureManager mgr, UUID[] ids, Location[] locs,
                                    int players, int movesPerTick) {
        mgr.tick(); // the plugin refreshes the cached clock once per tick
        for (int p = 0; p < players; p++) {
            for (int m = 0; m < movesPerTick; m++) {
                mgr.recordMovePacket(ids[p], 0.21f, 0f, 0.13f, 90f, 0f, true,
                        locs[p].getBlockX(), locs[p].getBlockZ());
            }
        }
    }
}
