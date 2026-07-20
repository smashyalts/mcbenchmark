import com.mcbench.capture.model.PlayerIndex;

import org.bukkit.Location;

import java.util.ArrayList;
import java.util.Arrays;
import java.util.List;
import java.util.UUID;

/**
 * MobAttrBench measures CaptureListener.nearestPlayer, which runs on the main
 * thread for every CreatureSpawnEvent and every EntityDeathEvent.
 *
 * It is a faithful model of the loop rather than a live call: Player/World cannot
 * be instantiated outside a running server, but every cost the real loop pays is
 * reproduced — the player-list copy getPlayers() returns, the fresh Location that
 * getLocation() allocates per player, and distanceSquared per player.
 *
 * The point is the shape: cost is O(online players) per mob event, so it grows
 * with the very number the benchmark is trying to scale.
 *
 *   java -cp ... MobAttrBench [players] [mobEventsPerSecond]
 */
public final class MobAttrBench {
    public static void main(String[] args) {
        int players = args.length > 0 ? Integer.parseInt(args[0]) : 1500;
        int mobEventsPerSec = args.length > 1 ? Integer.parseInt(args[1]) : 400;

        // Backing positions; getLocation() below allocates a copy, as Bukkit does.
        double[] px = new double[players];
        double[] pz = new double[players];
        for (int i = 0; i < players; i++) {
            px[i] = (i % 500) * 16;
            pz[i] = (i / 500) * 16;
        }

        int iters = 20_000;
        long[] ns = new long[iters];
        for (int warm = 0; warm < 5_000; warm++) {
            nearestPlayer(px, pz, players, warm * 13 % 8000, warm * 7 % 8000);
        }
        for (int i = 0; i < iters; i++) {
            long t0 = System.nanoTime();
            nearestPlayer(px, pz, players, i * 13 % 8000, i * 7 % 8000);
            ns[i] = System.nanoTime() - t0;
        }
        Arrays.sort(ns);
        double meanUs = Arrays.stream(ns).average().orElse(0) / 1000.0;
        double p99Us = ns[(int) (iters * 0.99)] / 1000.0;

        // Same question asked of the replacement: a 64-block spatial grid kept
        // current from the movement events capture already handles.
        PlayerIndex idx = new PlayerIndex();
        UUID[] uuids = new UUID[players];
        for (int i = 0; i < players; i++) {
            uuids[i] = new UUID(0x5EEDL, i);
            idx.update(uuids[i], px[i], 64, pz[i]);
        }
        long[] ns2 = new long[iters];
        for (int warm = 0; warm < 5_000; warm++) {
            idx.nearest(warm * 13 % 8000, 64, warm * 7 % 8000);
        }
        for (int i = 0; i < iters; i++) {
            long t0 = System.nanoTime();
            idx.nearest(i * 13 % 8000, 64, i * 7 % 8000);
            ns2[i] = System.nanoTime() - t0;
        }
        Arrays.sort(ns2);
        double meanUs2 = Arrays.stream(ns2).average().orElse(0) / 1000.0;
        double p99Us2 = ns2[(int) (iters * 0.99)] / 1000.0;
        double perTickMs2 = meanUs2 * mobEventsPerSec / 20.0 / 1000.0;

        // A mob event costs meanUs; how much of each 50 ms tick does that eat?
        double perTickMs = meanUs * mobEventsPerSec / 20.0 / 1000.0;

        System.out.println("=== nearestPlayer (main thread, per mob spawn/death) ===");
        System.out.printf("players=%d%n", players);
        System.out.printf("per call: mean %.1f us  p99 %.1f us%n", meanUs, p99Us);
        System.out.printf("at %d mob events/s -> %.2f ms per tick (%.1f%% of the 50 ms budget)%n",
                mobEventsPerSec, perTickMs, 100 * perTickMs / 50.0);
        System.out.println("=== PlayerIndex.nearest (the replacement) ===");
        System.out.printf("per call: mean %.2f us  p99 %.2f us%n", meanUs2, p99Us2);
        System.out.printf("at %d mob events/s -> %.3f ms per tick (%.2f%% of the 50 ms budget)%n",
                mobEventsPerSec, perTickMs2, 100 * perTickMs2 / 50.0);
        System.out.printf(">>> %.0fx faster%n", meanUs / Math.max(meanUs2, 1e-9));
    }

    /** Mirrors CaptureListener.nearestPlayer, including its per-player allocation. */
    private static Object nearestPlayer(double[] px, double[] pz, int players, double ex, double ez) {
        List<Location> online = new ArrayList<>(players); // getPlayers() copy
        for (int i = 0; i < players; i++) {
            online.add(new Location(null, px[i], 64, pz[i]));
        }
        Location entLoc = new Location(null, ex, 64, ez);
        Location best = null;
        double bestSq = 48 * 48;
        for (Location p : online) {
            // Location.distanceSquared refuses a null world outside a server, so
            // inline the arithmetic it performs after that check.
            double dx = p.getX() - entLoc.getX();
            double dy = p.getY() - entLoc.getY();
            double dz = p.getZ() - entLoc.getZ();
            double d = dx * dx + dy * dy + dz * dz;
            if (d <= bestSq) {
                bestSq = d;
                best = p;
            }
        }
        return best;
    }
}
