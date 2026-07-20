package com.mcbench.capture;

import com.mcbench.capture.io.CaptureLogWriter;
import com.mcbench.capture.model.PlayerIndex;

import com.github.retrooper.packetevents.PacketEvents;

import org.bukkit.configuration.file.FileConfiguration;
import org.bukkit.plugin.java.JavaPlugin;
import org.bukkit.scheduler.BukkitTask;

import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.Paths;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.concurrent.Executors;
import java.util.concurrent.ScheduledExecutorService;
import java.util.concurrent.TimeUnit;

/**
 * BenchCapturePlugin wires the capture pipeline together: it reads config,
 * starts the async WriterTask, and registers the event listener. On disable it
 * cancels the task and performs a final synchronous flush so no buffered events
 * are lost on shutdown.
 */
public final class BenchCapturePlugin extends JavaPlugin {
    private CaptureManager manager;
    private final List<CaptureLogWriter> writers = new ArrayList<>();
    private final List<WriterTask> writerTasks = new ArrayList<>();
    private ScheduledExecutorService flushPool;
    private BukkitTask tickClock;
    private PacketMovementListener packetListener;

    // NOTE: there is deliberately no onLoad() here, and nothing in this plugin
    // calls PacketEvents.setAPI(), load(), init() or terminate().
    //
    // Those calls belong to whoever *owns* the PacketEvents instance. We declare
    // packetevents as a hard depend, so the PacketEvents plugin owns it: its own
    // onLoad does setAPI(build(this)) + load(), its onEnable does init(), and its
    // onDisable does terminate(). Doing the same from here is not redundant, it
    // is destructive on a real server:
    //
    //   - setAPI() replaces the global instance, orphaning the one every other
    //     PacketEvents plugin already registered its listeners against;
    //   - init() re-injects channel handlers on top of the existing ones;
    //   - terminate() on our disable shuts PacketEvents down server-wide, so
    //     reloading this benchmark plugin would silently break the anticheat and
    //     every shop plugin while the server keeps running.
    //
    // Build/setAPI/init is the right pattern only when PacketEvents is shaded
    // inside the plugin. It is the wrong one here.

    @Override
    public void onEnable() {
        saveDefaultConfig();
        FileConfiguration cfg = getConfig();

        if (!cfg.getBoolean("capture.enabled", true)) {
            getLogger().info("capture disabled via config; plugin idle");
            return;
        }

        String outputPath = cfg.getString("capture.output_path",
                "/home/container/bench-capture/capture-logs");
        int flushIntervalTicks = cfg.getInt("capture.flush_interval_ticks", 20);
        int bufferPerPlayerKb = cfg.getInt("capture.buffer_per_player_kb", 32);
        boolean anonymize = cfg.getBoolean("capture.anonymize_players", true);
        boolean captureChat = cfg.getBoolean("capture.capture_chat", true);
        int maxCommandLength = cfg.getInt("capture.max_command_length", 256);
        // One event per login. Off only if item ids in the log are unwanted.
        boolean captureInventory = cfg.getBoolean("capture.capture_inventory", true);
        int schemaVersion = cfg.getInt("capture.schema_version", 1);
        String serverId = cfg.getString("capture.server_id", "paper-prod-1");
        String regionId = cfg.getString("capture.region_id", "");
        int rotateMinutes = cfg.getInt("capture.rotate_minutes", 5);
        int rotateMaxMb = cfg.getInt("capture.rotate_max_mb", 128);
        // One writer thread measured 240k events/sec with zero drops, ~8x what
        // 1500 moving players generate, so one is the honest default. Raise it
        // only if the stats line reports drops (slow disk, or far more players).
        int writerThreads = Math.max(1, Math.min(16, cfg.getInt("capture.writer_threads", 1)));
        // Retention ceiling for the whole capture directory. Capture writes ~13
        // compressed bytes per event, so 1500 players produce ~1.4 GB/hour;
        // without a cap the log grows until the disk is full, which stops the
        // server saving chunks, not merely capture. 0 disables pruning.
        long maxTotalBytes = (long) cfg.getInt("capture.max_total_mb", 20480) * 1024L * 1024L;

        Path outputDir = Paths.get(outputPath);
        try {
            Files.createDirectories(outputDir);
        } catch (IOException e) {
            getLogger().severe("cannot create output dir " + outputDir + ": " + e.getMessage());
            getServer().getPluginManager().disablePlugin(this);
            return;
        }

        long maxBufferBytes = (long) bufferPerPlayerKb * 1024L;
        manager = new CaptureManager(anonymize, maxBufferBytes, regionId, writerThreads);
        for (int i = 0; i < writerThreads; i++) {
            CaptureLogWriter w = new CaptureLogWriter(schemaVersion, serverId);
            writers.add(w);
            writerTasks.add(new WriterTask(manager, w, outputDir,
                    (long) rotateMinutes * 60_000L, (long) rotateMaxMb * 1024L * 1024L,
                    getLogger(), i, maxTotalBytes));
        }

        PlayerIndex index = new PlayerIndex();
        CaptureListener listener = new CaptureListener(manager, index, captureChat,
                maxCommandLength, captureInventory);
        getServer().getPluginManager().registerEvents(listener, this);

        // Movement comes from the wire, on Netty threads, not from Bukkit events.
        if (PacketEvents.getAPI() == null) {
            // Cannot happen with the hard depend in plugin.yml, but capture
            // without movement is worse than no capture: it looks like it worked.
            getLogger().severe("PacketEvents API unavailable; movement cannot be captured");
            getServer().getPluginManager().disablePlugin(this);
            return;
        }
        packetListener = new PacketMovementListener(manager, index);
        PacketEvents.getAPI().getEventManager().registerListener(packetListener);

        // Counts ticks so the writer can report the server's real TPS alongside
        // capture throughput. A capture is only as trustworthy as the server that
        // produced it, and Paper stays quiet until a tick overruns by two seconds.
        tickClock = getServer().getScheduler().runTaskTimer(this, manager::tick, 1L, 1L);

        // The writer runs on its own wall-clock scheduler, NOT Bukkit's.
        //
        // Bukkit schedules even asynchronous repeating tasks in server ticks, so
        // a 20-tick flush becomes a 10-second flush when the server drops to 2
        // TPS. That couples capture to server health backwards: the buffers fill
        // fastest exactly when they are drained slowest, and a load test starts
        // losing the events it exists to record. Measured on a 250-player
        // movement run: 1,727 events dropped and 2 flushes in 30 seconds.
        //
        // A plain ScheduledExecutorService keeps draining at a fixed real-time
        // rate no matter what the main thread is doing.
        long periodMs = Math.max(50L, flushIntervalTicks * 50L);
        AtomicInteger threadIds = new AtomicInteger();
        flushPool = Executors.newScheduledThreadPool(writerThreads, r -> {
            Thread t = new Thread(r, "BenchCapture-writer-" + threadIds.getAndIncrement());
            t.setDaemon(true);
            return t;
        });
        for (int i = 0; i < writerTasks.size(); i++) {
            // Stagger starts so the shards do not all compress and fsync in the
            // same instant.
            long stagger = periodMs * i / Math.max(1, writerTasks.size());
            flushPool.scheduleWithFixedDelay(writerTasks.get(i), periodMs + stagger, periodMs,
                    TimeUnit.MILLISECONDS);
        }

        getLogger().info("BenchCapture enabled: writing to " + outputDir
                + " (flush every " + periodMs + " ms, " + writerThreads
                + " writer thread(s), server_id=" + serverId + ")");
    }

    @Override
    public void onDisable() {
        // Stop capturing before draining, so the final flush sees a settled set
        // of rings. Unregister only our own listener — PacketEvents itself stays
        // up for the rest of the server.
        if (packetListener != null && PacketEvents.getAPI() != null) {
            PacketEvents.getAPI().getEventManager().unregisterListener(packetListener);
            packetListener = null;
        }
        if (flushPool != null) {
            flushPool.shutdown();
            try {
                // Let an in-flight flush finish before the final drain below,
                // so the two cannot interleave on the writer.
                flushPool.awaitTermination(5, TimeUnit.SECONDS);
            } catch (InterruptedException ie) {
                Thread.currentThread().interrupt();
            }
            flushPool = null;
        }
        if (tickClock != null) {
            tickClock.cancel();
            tickClock = null;
        }
        for (WriterTask t : writerTasks) {
            try {
                t.flushOnce(); // final drain of anything still buffered
            } catch (IOException e) {
                getLogger().warning("final flush failed: " + e.getMessage());
            }
        }
        if (!writerTasks.isEmpty()) {
            writerTasks.get(0).logSummary();
        }
        for (CaptureLogWriter w : writers) {
            try {
                w.close();
                w.dispose();
            } catch (IOException e) {
                getLogger().warning("writer close failed: " + e.getMessage());
            }
        }
        writerTasks.clear();
        writers.clear();
        getLogger().info("BenchCapture disabled");
    }
}
