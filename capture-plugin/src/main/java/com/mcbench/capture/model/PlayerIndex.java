package com.mcbench.capture.model;

import java.util.Map;
import java.util.UUID;
import java.util.concurrent.ConcurrentHashMap;

/**
 * PlayerIndex answers "which player is nearest this position" in roughly
 * constant time, replacing a scan over every online player.
 *
 * Mob spawn and death events have no player attached, so capture attributes them
 * to a nearby player's stream. Doing that by scanning
 * {@code world.getPlayers()} costs O(online players) per mob event — and mob
 * event rates themselves rise with player count, so the total cost grows
 * quadratically with server size. Measured on the old path: 25 us per mob event
 * at 1500 players, versus 3.7 us at 100.
 *
 * Instead, players are bucketed into 64-block cells, updated in place from the
 * movement events capture already receives. A lookup scans the 3x3 cells around
 * the entity, which holds a handful of players regardless of how many are
 * online.
 *
 * Each player's entry is written only by that connection's Netty thread (Netty
 * pins a channel to one event loop), and read by the main thread for mob
 * attribution. The maps are concurrent and the coordinate fields volatile, so
 * readers see a consistent position without locking.
 */
public final class PlayerIndex {
    private static final int CELL_SHIFT = 6; // 64-block cells
    /** Attribution radius, matching the previous 48-block behaviour. */
    private static final double MAX_DIST_SQ = 48 * 48;

    /** One player's live position. Mutated in place so movement allocates nothing. */
    private static final class Entry {
        final UUID id;
        /**
         * Volatile: written by the connection's Netty thread when a movement
         * packet arrives, read by the main thread during mob attribution. A
         * non-volatile double can tear across threads, which would put a player
         * at a coordinate they were never at.
         */
        volatile double x, y, z;
        volatile long cell;

        Entry(UUID id) {
            this.id = id;
            this.cell = Long.MIN_VALUE; // "not in any cell yet"
        }
    }

    private final Map<UUID, Entry> byPlayer = new ConcurrentHashMap<>();
    private final Map<Long, Map<UUID, Entry>> byCell = new ConcurrentHashMap<>();

    private static long cellKey(double x, double z) {
        long cx = (long) Math.floor(x) >> CELL_SHIFT;
        long cz = (long) Math.floor(z) >> CELL_SHIFT;
        return (cx << 32) ^ (cz & 0xFFFFFFFFL);
    }

    /**
     * Records a player's current position. The common case — a player who has
     * not crossed a cell boundary — is a map lookup and three field stores, with
     * no allocation and no map mutation.
     */
    public void update(UUID id, double x, double y, double z) {
        Entry e = byPlayer.get(id);
        if (e == null) {
            // computeIfAbsent, not get-then-put: two threads do race here. The
            // main thread seeds a player's position at join while that player's
            // Netty thread may already be handling their first movement packet.
            // With get-then-put both create an Entry, one wins byPlayer, and the
            // loser stays in byCell forever — a slow leak that also makes
            // nearest() return players who have since left.
            e = byPlayer.computeIfAbsent(id, Entry::new);
        }
        e.x = x;
        e.y = y;
        e.z = z;
        long cell = cellKey(x, z);
        if (cell != e.cell) {
            if (e.cell != Long.MIN_VALUE) {
                Map<UUID, Entry> old = byCell.get(e.cell);
                if (old != null) {
                    old.remove(id);
                    if (old.isEmpty()) {
                        byCell.remove(e.cell, old);
                    }
                }
            }
            byCell.computeIfAbsent(cell, k -> new ConcurrentHashMap<>()).put(id, e);
            e.cell = cell;
        }
    }

    public void remove(UUID id) {
        Entry e = byPlayer.remove(id);
        if (e == null || e.cell == Long.MIN_VALUE) {
            return;
        }
        Map<UUID, Entry> cell = byCell.get(e.cell);
        if (cell != null) {
            cell.remove(id);
            if (cell.isEmpty()) {
                byCell.remove(e.cell, cell);
            }
        }
    }

    /**
     * Returns the nearest player within 48 blocks, or null. Scans the 3x3 block
     * of cells around the position, which is enough because the attribution
     * radius (48) is smaller than the cell size (64).
     */
    public UUID nearest(double x, double y, double z) {
        long cx = (long) Math.floor(x) >> CELL_SHIFT;
        long cz = (long) Math.floor(z) >> CELL_SHIFT;
        UUID best = null;
        double bestSq = MAX_DIST_SQ;
        for (long ox = -1; ox <= 1; ox++) {
            for (long oz = -1; oz <= 1; oz++) {
                Map<UUID, Entry> cell = byCell.get(((cx + ox) << 32) ^ ((cz + oz) & 0xFFFFFFFFL));
                if (cell == null) {
                    continue;
                }
                for (Entry e : cell.values()) {
                    double dx = e.x - x;
                    double dy = e.y - y;
                    double dz = e.z - z;
                    double d = dx * dx + dy * dy + dz * dz;
                    if (d <= bestSq) {
                        bestSq = d;
                        best = e.id;
                    }
                }
            }
        }
        return best;
    }

    public int size() { return byPlayer.size(); }
}
