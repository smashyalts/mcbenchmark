import com.mcbench.capture.CaptureManager;
import com.mcbench.capture.WriterTask;
import com.mcbench.capture.io.CaptureLogWriter;
import com.mcbench.capture.model.PlayerIndex;

import java.lang.management.ManagementFactory;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.Arrays;
import java.util.UUID;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.Executors;
import java.util.concurrent.ScheduledExecutorService;
import java.util.concurrent.TimeUnit;
import java.util.logging.Logger;

/**
 * PacketPathBench measures movement capture where it now actually runs: on many
 * Netty event-loop threads, concurrently, with no main thread involved.
 *
 * MainThreadBench no longer covers this. It drives every event from one thread,
 * which was correct when movement came from PlayerMoveEvent, and is now
 * misleading in both directions: it misses cross-thread contention (the LongAdder
 * counters, the session map, the shared writer) and it charges the cost to a
 * thread that no longer does the work.
 *
 * Netty pins a channel to one event loop for its lifetime, so players are
 * partitioned across threads once and stay put — the same partitioning is
 * modelled here. Each producer thread paces itself to the target packet rate so
 * the writer drains against a realistic offered load rather than an unbounded
 * burst.
 *
 * The reported per-event cost is capture only. It excludes what PacketEvents
 * spends decoding the packet before calling us, which is the other half of the
 * question and has to be measured on a live server (see docs/CAPTURE-COST.md).
 *
 *   java -cp ... PacketPathBench [players] [seconds] [packetsPerSecPerPlayer] [nettyThreads] [writerThreads]
 */
public final class PacketPathBench {
    /**
     * -Dbench.index=false skips the spatial-index update, isolating the cost of
     * the ring write alone. The index exists only so mob events can be attributed
     * to a nearby player, so if it dominates, that feature is what to question —
     * not the capture path.
     */
    private static final boolean INDEX = !"false".equals(System.getProperty("bench.index"));

    public static void main(String[] args) throws Exception {
        int players = args.length > 0 ? Integer.parseInt(args[0]) : 1500;
        int seconds = args.length > 1 ? Integer.parseInt(args[1]) : 20;
        int rate = args.length > 2 ? Integer.parseInt(args[2]) : 20;
        int netty = args.length > 3 ? Integer.parseInt(args[3])
                : Math.max(1, Runtime.getRuntime().availableProcessors());
        int writers = args.length > 4 ? Integer.parseInt(args[4]) : 1;

        Path dir = Files.createTempDirectory("benchcapture-packet");
        Logger log = Logger.getLogger("bench");
        CaptureManager mgr = new CaptureManager(true, 32L * 1024, "", writers);
        PlayerIndex index = new PlayerIndex();

        UUID[] ids = new UUID[players];
        for (int i = 0; i < players; i++) {
            ids[i] = new UUID(0x5EEDL, i);
            mgr.onJoin(ids[i]);
        }

        java.util.List<CaptureLogWriter> writerList = new java.util.ArrayList<>();
        java.util.List<WriterTask> taskList = new java.util.ArrayList<>();
        for (int i = 0; i < writers; i++) {
            CaptureLogWriter w = new CaptureLogWriter(1, "packetbench");
            writerList.add(w);
            taskList.add(new WriterTask(mgr, w, dir, 0, 0, log, i));
        }
        ScheduledExecutorService sched = Executors.newScheduledThreadPool(writers, r -> {
            Thread t = new Thread(r, "capture-writer");
            t.setDaemon(true);
            return t;
        });
        for (WriterTask t : taskList) {
            sched.scheduleAtFixedRate(t, 200, 1000, TimeUnit.MILLISECONDS);
        }

        var tmx = (com.sun.management.ThreadMXBean) ManagementFactory.getThreadMXBean();
        long periodNanos = 1_000_000_000L / rate;
        CountDownLatch ready = new CountDownLatch(netty);
        CountDownLatch go = new CountDownLatch(1);
        Producer[] producers = new Producer[netty];
        Thread[] threads = new Thread[netty];

        for (int t = 0; t < netty; t++) {
            producers[t] = new Producer(mgr, index, ids, t, netty, seconds, periodNanos, ready, go, tmx);
            threads[t] = new Thread(producers[t], "netty-" + t);
            threads[t].start();
        }
        ready.await();
        // Warmup runs unpaced and leaves the rings full, so measuring from here
        // would count a startup burst that says nothing about steady state. Let
        // the writer drain first, then start counting drops.
        Thread.sleep(2000);
        long dropBefore = mgr.totalDropped();
        long wall0 = System.nanoTime();
        go.countDown();
        for (Thread t : threads) {
            t.join();
        }
        long wallNanos = System.nanoTime() - wall0;

        sched.shutdown();
        sched.awaitTermination(5, TimeUnit.SECONDS);
        for (WriterTask t : taskList) {
            t.flushOnce();
        }
        for (CaptureLogWriter w : writerList) {
            w.close();
        }

        long produced = 0;
        long busyNanos = 0;
        long allocBytes = 0;
        long[] all = new long[0];
        for (Producer p : producers) {
            produced += p.count;
            busyNanos += p.busyNanos;
            allocBytes += p.allocBytes;
            int n = all.length;
            all = Arrays.copyOf(all, n + p.samples.length);
            System.arraycopy(p.samples, 0, all, n, p.samples.length);
        }
        Arrays.sort(all);
        long dropped = mgr.totalDropped() - dropBefore;
        double secs = wallNanos / 1e9;

        System.out.println("=== BenchCapture packet-path cost (off main thread) ===");
        System.out.printf("players=%d  packets/s/player=%d  netty threads=%d  writers=%d  cores=%d%n",
                players, rate, netty, writers, Runtime.getRuntime().availableProcessors());
        System.out.printf("produced=%,d in %.1fs (%,.0f ev/s offered)  dropped=%,d (%.3f%%)"
                        + "  off-thread=%,d%n",
                produced, secs, produced / secs, dropped, 100.0 * dropped / produced,
                mgr.offThreadDropped());
        System.out.printf("per-event capture: mean %.0f ns   per-batch mean ns/event:"
                        + " p50 %d  p99 %d  max %,d%n",
                (double) busyNanos / produced,
                all[all.length / 2], all[(int) (all.length * 0.99)], all[all.length - 1]);
        System.out.printf("alloc: %.1f B/event  (%.2f MiB/s across all producers)%n",
                (double) allocBytes / produced, allocBytes / 1048576.0 / secs);

        // What one Netty thread's share of this costs, as a fraction of its time.
        double perThreadBusy = (double) busyNanos / netty;
        System.out.printf("per-netty-thread occupancy: %.4f%% of wall time%n",
                100.0 * perThreadBusy / wallNanos);
        System.out.printf("written=%,d events, %,d KiB%n",
                mgr.writtenEvents(), mgr.writtenBytes() / 1024);

        for (Path p : Files.list(dir).toList()) {
            Files.deleteIfExists(p);
        }
        Files.deleteIfExists(dir);
    }

    /** Cost of the surrounding System.nanoTime() pair, measured the same way. */
    private static long clockCost() {
        long[] s = new long[200_000];
        for (int i = 0; i < s.length; i++) {
            long t0 = System.nanoTime();
            s[i] = System.nanoTime() - t0;
        }
        Arrays.sort(s);
        return s[s.length / 2];
    }

    /**
     * One simulated Netty event loop, owning a fixed slice of the players — the
     * same shape as the real thing, where a channel never migrates between event
     * loops.
     */
    private static final class Producer implements Runnable {
        private final CaptureManager mgr;
        private final PlayerIndex index;
        private final UUID[] ids;
        private final int offset, stride, seconds;
        private final long periodNanos;
        private final CountDownLatch ready, go;
        private final com.sun.management.ThreadMXBean tmx;
        long count, busyNanos, allocBytes;
        long[] samples = new long[0];

        Producer(CaptureManager mgr, PlayerIndex index, UUID[] ids, int offset, int stride,
                 int seconds, long periodNanos, CountDownLatch ready, CountDownLatch go,
                 com.sun.management.ThreadMXBean tmx) {
            this.mgr = mgr;
            this.index = index;
            this.ids = ids;
            this.offset = offset;
            this.stride = stride;
            this.seconds = seconds;
            this.periodNanos = periodNanos;
            this.ready = ready;
            this.go = go;
            this.tmx = tmx;
        }

        @Override
        public void run() {
            int mine = 0;
            for (int i = offset; i < ids.length; i += stride) {
                mine++;
            }
            if (mine == 0) {
                ready.countDown();
                return;
            }
            // Warm up so the timed section runs JIT'd code, not the interpreter.
            for (int w = 0; w < 200; w++) {
                emitRound(0);
            }
            // Warmup work is not part of the measurement.
            count = 0;
            busyNanos = 0;
            ready.countDown();
            try {
                go.await();
            } catch (InterruptedException e) {
                Thread.currentThread().interrupt();
                return;
            }

            long self = Thread.currentThread().threadId();
            long a0 = tmx.getThreadAllocatedBytes(self);
            int rounds = seconds * (int) (1_000_000_000L / periodNanos);
            samples = new long[rounds];
            int si = 0;
            long next = System.nanoTime();
            for (int r = 0; r < rounds; r++) {
                si = emitRound(si);
                next += periodNanos;
                long sleep = next - System.nanoTime();
                if (sleep > 0) {
                    java.util.concurrent.locks.LockSupport.parkNanos(sleep);
                }
            }
            allocBytes = tmx.getThreadAllocatedBytes(self) - a0;
            samples = Arrays.copyOf(samples, si);
        }

        /**
         * One packet per owned player.
         *
         * The round is timed as a whole rather than per event. A System.nanoTime()
         * pair costs roughly 200 ns on this platform — comparable to the work
         * being measured — so timing each event individually more than doubles the
         * reported cost. The sample recorded per round is the round's mean, which
         * is also the honest unit for a Netty event loop: it wakes, drains a batch
         * of packets, and goes back to sleep.
         */
        private int emitRound(int si) {
            long t0 = System.nanoTime();
            int n = 0;
            for (int i = offset; i < ids.length; i += stride) {
                double x = (i % 500) * 16 + (count & 15);
                double z = (i / 500) * 16 + (count & 7);
                if (INDEX) {
                    index.update(ids[i], x, 64, z);
                }
                mgr.recordMovePacket(ids[i], 0.12f, 0f, -0.08f, 91.5f, 3.25f, true,
                        (int) x, (int) z);
                count++;
                n++;
            }
            long dt = System.nanoTime() - t0;
            busyNanos += dt;
            if (n > 0 && si < samples.length) {
                samples[si++] = dt / n;
            }
            return si;
        }
    }

}
