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
        /** Set once, under the entry's lock, when the player quits. */
        volatile boolean gone;

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
     * Registers a joining player. Called once per session, from the main thread.
     *
     * Creation is deliberately confined to this method: see {@link #update}.
     */
    public void add(UUID id, double x, double y, double z) {
        // computeIfAbsent, not get-then-put: with get-then-put a re-join racing a
        // stale entry leaves two, one of which stays in byCell forever.
        byPlayer.computeIfAbsent(id, Entry::new);
        update(id, x, y, z);
    }

    /**
     * Records a player's current position. The common case — a player who has
     * not crossed a cell boundary — is a map lookup and three field stores, with
     * no allocation and no map mutation.
     *
     * An unknown player is ignored rather than created, which is what makes
     * {@link #remove} final. The packet listener checks the session before
     * calling here, but those are two separate steps: a player can quit in
     * between, and a movement packet already in flight on the Netty thread then
     * arrives after the main thread has evicted them. Creating an entry there
     * would re-insert a player who is gone, with nothing left to ever remove it —
     * the index would grow for the life of the plugin and keep attributing mob
     * events to players who logged off hours ago.
     */
    public void update(UUID id, double x, double y, double z) {
        Entry e = byPlayer.get(id);
        if (e == null) {
            return;
        }
        e.x = x;
        e.y = y;
        e.z = z;
        long cell = cellKey(x, z);
        if (cell != e.cell) {
            moveCell(id, e, cell);
        }
    }

    /**
     * Moves an entry between cells, under the entry's own lock.
     *
     * This player's position is not written by one thread after all. Movement
     * packets arrive on the connection's Netty thread, but the main thread also
     * calls {@link #update} — at join, and on every teleport, respawn and world
     * change, which is precisely when the cell changes. Two threads relocating
     * the same entry concurrently can interleave so that the removal from the old
     * cell runs after the insertion into the new one, leaving the entry in a cell
     * {@code e.cell} no longer names. Nothing ever removes it again: quit only
     * looks at {@code e.cell}. So the entry leaks for the lifetime of the plugin,
     * and {@link #nearest} keeps attributing mobs to a player who left, at a
     * position they have not been at since the teleport.
     *
     * The lock is only taken when the cell actually changes — once per 64 blocks
     * travelled — so the hot path, a player moving inside their own cell, is
     * still three field stores and a comparison.
     */
    private void moveCell(UUID id, Entry e, long cell) {
        synchronized (e) {
            if (e.gone) {
                // The player quit between this thread's byPlayer lookup and here.
                // Inserting now would put them back into a cell with no entry in
                // byPlayer to ever find them again.
                return;
            }
            if (cell == e.cell) {
                return; // another thread got here first
            }
            detach(id, e.cell);
            byCell.computeIfAbsent(cell, k -> new ConcurrentHashMap<>()).put(id, e);
            e.cell = cell;
        }
    }

    /** Removes a player from one cell, dropping the cell if it empties. */
    private void detach(UUID id, long cell) {
        if (cell == Long.MIN_VALUE) {
            return;
        }
        Map<UUID, Entry> old = byCell.get(cell);
        if (old != null) {
            old.remove(id);
            if (old.isEmpty()) {
                byCell.remove(cell, old);
            }
        }
    }

    public void remove(UUID id) {
        Entry e = byPlayer.remove(id);
        if (e == null) {
            return;
        }
        // Same lock as moveCell: a movement packet still in flight on the Netty
        // thread must not re-insert this entry into a cell after we have taken it
        // out of the one it currently names.
        synchronized (e) {
            e.gone = true;
            detach(id, e.cell);
            e.cell = Long.MIN_VALUE;
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

    /**
     * Total entries across all cells, which must equal {@link #size}.
     *
     * A player is one entry in one cell. Any excess is a player stranded in a
     * cell their entry no longer names, which nothing will ever remove — and it
     * is invisible from the outside, because the stale entry points at the same
     * object as the live one and therefore reports the player's real, current
     * position. Counting is the only way to see it.
     */
    public int cellEntries() {
        int n = 0;
        for (Map<UUID, Entry> cell : byCell.values()) {
            n += cell.size();
        }
        return n;
    }
}
