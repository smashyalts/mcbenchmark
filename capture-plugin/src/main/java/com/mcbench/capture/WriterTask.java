package com.mcbench.capture;

import com.mcbench.capture.io.CaptureLogWriter;
import com.mcbench.capture.model.PlayerSession;
import com.mcbench.capture.model.RawEvent;

import java.io.IOException;
import java.nio.file.Path;
import java.time.ZoneId;
import java.time.ZonedDateTime;
import java.time.format.DateTimeFormatter;
import java.util.ArrayList;
import java.util.List;
import java.util.logging.Level;
import java.util.logging.Logger;

/**
 * WriterTask drains every player session's queue, writes one compressed frame
 * per flush, and rotates the output file by age or size. It runs on an async
 * scheduler thread so compression and disk IO never touch the server main
 * thread; the session queues are lock-free, so draining is safe concurrently
 * with main-thread event production.
 */
public final class WriterTask implements Runnable {
    private static final DateTimeFormatter FILE_FMT =
            DateTimeFormatter.ofPattern("yyyyMMdd-HHmmss");

    private final CaptureManager manager;
    private final CaptureLogWriter writer;
    private final Path outputDir;
    private final Logger log;
    private final long rotateAfterMs;
    private final long rotateMaxBytes;
    /** Which slice of sessions this writer owns; -1 means "all of them". */
    private final int shard;

    private long fileOpenedAt;

    // Reused across flushes; this task is the only thread that touches them.
    // The frame buffer settles at roughly one flush interval of events and is
    // never reallocated after that.
    private final com.mcbench.capture.io.ByteWriter frame =
            new com.mcbench.capture.io.ByteWriter(512 * 1024);
    private final long[] bounds = new long[2];
    /** Retention ceiling for the whole capture directory; 0 disables pruning. */
    private final long maxTotalBytes;
    private long prunedFiles;
    private long prunedBytes;
    private final byte[] regionIdUtf8;

    // Throughput accounting (writer thread only).
    private final long startedAtMs = System.currentTimeMillis();
    private long framesWritten;
    private long eventsWritten;
    private long bytesWritten;
    private long flushCount;
    private long lastStatsMs = System.currentTimeMillis();
    private long lastStatsEvents;
    private long lastStatsTicks;

    public WriterTask(CaptureManager manager, CaptureLogWriter writer, Path outputDir,
                      long rotateAfterMs, long rotateMaxBytes, Logger log) {
        this(manager, writer, outputDir, rotateAfterMs, rotateMaxBytes, log, 0);
    }

    public WriterTask(CaptureManager manager, CaptureLogWriter writer, Path outputDir,
                      long rotateAfterMs, long rotateMaxBytes, Logger log, int shard) {
        this(manager, writer, outputDir, rotateAfterMs, rotateMaxBytes, log, shard, 0L);
    }

    public WriterTask(CaptureManager manager, CaptureLogWriter writer, Path outputDir,
                      long rotateAfterMs, long rotateMaxBytes, Logger log, int shard,
                      long maxTotalBytes) {
        this.maxTotalBytes = maxTotalBytes;
        this.shard = shard;
        this.manager = manager;
        this.writer = writer;
        this.outputDir = outputDir;
        this.rotateAfterMs = rotateAfterMs;
        this.rotateMaxBytes = rotateMaxBytes;
        this.log = log;
        this.regionIdUtf8 = manager.regionId()
                .getBytes(java.nio.charset.StandardCharsets.UTF_8);
    }

    @Override
    public void run() {
        try {
            flushOnce();
        } catch (Throwable t) {
            // Never let an exception kill the repeating task.
            log.log(Level.WARNING, "capture flush failed", t);
        }
    }

    /**
     * Drains all sessions and writes a frame; public so onDisable can final-flush.
     *
     * The batch is encoded straight into a reusable buffer rather than
     * materialised as a list of RawEvents. At 30,000 events/sec the old path
     * produced enough garbage to trigger a young GC every few seconds, and a
     * young GC is stop-the-world: writer-thread allocation stalls the tick too.
     */
    public synchronized void flushOnce() throws IOException {
        frame.reset();
        bounds[0] = Long.MAX_VALUE;
        bounds[1] = Long.MIN_VALUE;
        int count = 0;
        for (PlayerSession s : manager.activeSessions(shard)) {
            count += s.encodeTo(frame, regionIdUtf8, bounds);
        }
        // Players who left since the last flush still hold their final events.
        // They receive no further writes, so one drain empties them for good.
        PlayerSession gone;
        while ((gone = manager.departedSessions(shard).poll()) != null) {
            count += gone.encodeTo(frame, regionIdUtf8, bounds);
        }
        if (count == 0) {
            maybeRotateIdle();
            return;
        }
        ensureOpen();
        if (shouldRotate()) {
            rotate();
        }
        // Wall-clock is derived here rather than read from the clock per event.
        long epoch = manager.startEpochMs();
        long startMs = epoch + bounds[0] / 1000L;
        long endMs = epoch + bounds[1] / 1000L;
        long before = writer.bytesWritten();
        writer.writeFrame(frame, count, startMs, endMs);
        framesWritten++;
        eventsWritten += count;
        long deltaBytes = writer.bytesWritten() - before;
        bytesWritten += deltaBytes;
        manager.addWritten(count, 1, deltaBytes);
        maybeLogStats();
    }

    /** Logs capture throughput and drop counts roughly every 10s. */
    private void maybeLogStats() {
        flushCount++;
        long now = System.currentTimeMillis();
        if (now - lastStatsMs < 10_000) {
            return;
        }
        if (shard != 0) {
            // Counters below are server-wide; one shard reporting them is enough.
            return;
        }
        double windowSec = (now - lastStatsMs) / 1000.0;
        long totalEvents = manager.writtenEvents();
        long windowEvents = totalEvents - lastStatsEvents;
        double eps = windowSec > 0 ? windowEvents / windowSec : 0;
        long dropped = manager.totalDropped();
        long offThread = manager.offThreadDropped();
        long ticksNow = manager.ticks();
        double tps = windowSec > 0 ? (ticksNow - lastStatsTicks) / windowSec : 0;
        log.info(String.format(
                "capture stats: %,d events written (%,.0f ev/s), %,d frames, %,d KiB, "
                        + "dropped=%,d%s, players=%d, tps=%.1f, writers=%d",
                totalEvents, eps, manager.writtenFrames(), manager.writtenBytes() / 1024, dropped,
                offThread > 0 ? ", OFF-THREAD=" + offThread : "", playerCount(), tps,
                manager.shards()));
        if (prunedFiles > 0) {
            // Retention deleting capture data must never be silent: a run that
            // quietly lost its first hour looks identical to one that did not.
            log.info(String.format("capture retention: pruned %,d file(s), %,d MiB freed"
                    + " (cap %,d MiB)", prunedFiles, prunedBytes / 1048576L,
                    maxTotalBytes / 1048576L));
        }
        lastStatsMs = now;
        lastStatsEvents = totalEvents;
        lastStatsTicks = ticksNow;
    }

    private int playerCount() {
        int n = 0;
        for (@SuppressWarnings("unused") PlayerSession s : manager.activeSessions()) {
            n++;
        }
        return n;
    }

    /** Final throughput summary, logged on disable. */
    public void logSummary() {
        double sec = (System.currentTimeMillis() - startedAtMs) / 1000.0;
        long total = manager.writtenEvents();
        log.info(String.format(
                "capture summary: %,d events, %,d frames, %,d KiB over %.1fs (%,.0f ev/s avg), dropped=%,d",
                total, manager.writtenFrames(), manager.writtenBytes() / 1024, sec,
                sec > 0 ? total / sec : 0, manager.totalDropped()));
    }

    private void ensureOpen() throws IOException {
        if (!writer.isOpen()) {
            rotate();
        }
    }

    private void maybeRotateIdle() throws IOException {
        if (writer.isOpen() && rotateAfterMs > 0
                && System.currentTimeMillis() - fileOpenedAt >= rotateAfterMs) {
            rotate();
        }
    }

    private boolean shouldRotate() {
        long age = System.currentTimeMillis() - fileOpenedAt;
        return (rotateAfterMs > 0 && age >= rotateAfterMs)
                || (rotateMaxBytes > 0 && writer.bytesWritten() >= rotateMaxBytes);
    }

    /**
     * Deletes the oldest capture files until the directory fits the retention
     * budget. Runs on rotate, on the writer thread.
     *
     * Without this, capture grows without bound: files rotate but nothing ever
     * removes them. Measured at 13 compressed bytes per event, 1500 players
     * produce ~1.4 GB/hour and ~34 GB/day. On a container with a disk quota that
     * does not merely stop capture — a full disk stops the server writing chunks,
     * so an unbounded capture log takes the server down with it.
     *
     * Files another shard currently has open are never candidates: on Linux the
     * unlink would succeed and that writer would go on filling an unreachable
     * inode, losing the data with no error anywhere.
     */
    private void pruneOldFiles() {
        if (maxTotalBytes <= 0) {
            return;
        }
        try (java.util.stream.Stream<Path> files = java.nio.file.Files.list(outputDir)) {
            java.util.List<Path> caps = files
                    .filter(p -> p.getFileName().toString().startsWith("raw-")
                            && p.getFileName().toString().endsWith(".bin"))
                    .filter(p -> !manager.isOpen(p))
                    .sorted(java.util.Comparator.comparingLong(WriterTask::lastModified))
                    .collect(java.util.stream.Collectors.toList());
            long total = 0;
            for (Path p : caps) {
                total += sizeOf(p);
            }
            for (Path p : caps) {
                if (total <= maxTotalBytes) {
                    break;
                }
                long sz = sizeOf(p);
                try {
                    java.nio.file.Files.delete(p);
                    total -= sz;
                    prunedFiles++;
                    prunedBytes += sz;
                } catch (IOException e) {
                    log.warning("capture: could not prune " + p + ": " + e.getMessage());
                }
            }
        } catch (IOException e) {
            log.warning("capture: retention scan failed: " + e.getMessage());
        }
    }

    private static long lastModified(Path p) {
        try {
            return java.nio.file.Files.getLastModifiedTime(p).toMillis();
        } catch (IOException e) {
            return 0L; // unreadable: treat as oldest so it is pruned first
        }
    }

    private static long sizeOf(Path p) {
        try {
            return java.nio.file.Files.size(p);
        } catch (IOException e) {
            return 0L;
        }
    }

    private void rotate() throws IOException {
        // Each shard writes its own file; trace-compiler reads the whole
        // directory and regroups by (playerId, sessionSeq), so the split is
        // invisible downstream.
        String name = "raw-" + ZonedDateTime.now(ZoneId.systemDefault()).format(FILE_FMT)
                + "-s" + shard + ".bin";
        Path path = outputDir.resolve(name);
        Path prev = writer.currentPath();
        writer.open(path);
        manager.fileOpened(prev, path);
        fileOpenedAt = System.currentTimeMillis();
        log.info("capture: writing to " + path);
        // Prune after opening the new file, so the file just closed is a
        // candidate and the one just opened is protected.
        pruneOldFiles();
    }
}
