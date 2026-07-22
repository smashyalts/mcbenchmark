package com.mcbench.capture.model;

import com.mcbench.capture.io.ByteWriter;

/**
 * Payloads builds the per-kind RawEvent payload byte arrays. Each method's
 * layout matches the corresponding Go encoder in package rawevent
 * (docs/FORMAT.md section 2.2).
 */
public final class Payloads {
    private Payloads() {}

    public static byte[] move(float dx, float dy, float dz, float yaw, float pitch, boolean onGround) {
        ByteWriter w = new ByteWriter();
        w.float32LE(dx);
        w.float32LE(dy);
        w.float32LE(dz);
        w.float32LE(yaw);
        w.float32LE(pitch);
        w.bool(onGround);
        return w.toByteArray();
    }

    public static byte[] toggle(boolean on) {
        return new byte[] { (byte) (on ? 1 : 0) };
    }

    public static byte[] dig(int action, int x, int y, int z, int face) {
        ByteWriter w = new ByteWriter();
        w.varInt(action);
        w.varInt(x);
        w.varInt(y);
        w.varInt(z);
        w.varInt(face);
        return w.toByteArray();
    }

    public static byte[] place(int x, int y, int z, int face, int hand) {
        ByteWriter w = new ByteWriter();
        w.varInt(x);
        w.varInt(y);
        w.varInt(z);
        w.varInt(face);
        w.varInt(hand);
        return w.toByteArray();
    }

    public static byte[] useItem(int hand, int itemId) {
        ByteWriter w = new ByteWriter();
        w.varInt(hand);
        w.varInt(itemId);
        return w.toByteArray();
    }

    /**
     * What was attacked or interacted with: the entity's registry key, then the
     * hand used.
     *
     * The key ("minecraft:zombie"), not EntityType.ordinal(). The ordinal is an
     * enum position — it bears no relation to the protocol's entity type id,
     * shifts whenever an entity is added to the enum, and so could never be
     * matched against anything the replay client sees on the wire. It made the
     * captured target unusable, which is why attacks were replayed as a bare arm
     * swing: an animation with no damage, no aggro, no drops and no XP behind it.
     */
    public static byte[] entityRef(String typeKey, int hand) {
        ByteWriter w = new ByteWriter();
        w.string(typeKey == null ? "" : typeKey);
        w.varInt(hand);
        return w.toByteArray();
    }

    /** Player-inventory or location-less container: no block position. */
    public static byte[] invOpen(int containerType) {
        ByteWriter w = new ByteWriter();
        w.varInt(containerType);
        w.bool(false); // has_pos
        return w.toByteArray();
    }

    /** Block container: records its position so replay can trigger the open. */
    public static byte[] invOpen(int containerType, int x, int y, int z) {
        ByteWriter w = new ByteWriter();
        w.varInt(containerType);
        w.bool(true); // has_pos
        w.varInt(x);
        w.varInt(y);
        w.varInt(z);
        return w.toByteArray();
    }

    public static byte[] invClick(int windowId, int slot, int button, int clickType) {
        ByteWriter w = new ByteWriter();
        w.varInt(windowId);
        w.varInt(slot);
        w.varInt(button);
        w.varInt(clickType);
        return w.toByteArray();
    }

    public static byte[] invClose(int windowId) {
        ByteWriter w = new ByteWriter();
        w.varInt(windowId);
        return w.toByteArray();
    }

    public static byte[] command(String command) {
        ByteWriter w = new ByteWriter();
        w.string(command);
        return w.toByteArray();
    }

    public static byte[] mobSpawn(int entityType, String tag) {
        ByteWriter w = new ByteWriter();
        w.varInt(entityType);
        w.string(tag == null ? "" : tag);
        return w.toByteArray();
    }

    public static byte[] mobDespawn(int entityType, int reason) {
        ByteWriter w = new ByteWriter();
        w.varInt(entityType);
        w.varInt(reason);
        return w.toByteArray();
    }

    public static byte[] marker(String marker) {
        ByteWriter w = new ByteWriter();
        w.string(marker == null ? "" : marker);
        return w.toByteArray();
    }

    /**
     * The player's inventory at login: the selected hotbar slot, then one entry
     * per non-empty stack.
     *
     * Takes parallel arrays rather than Bukkit types so payload encoding stays in
     * one Bukkit-free place, matching every other payload here. Slots are Bukkit
     * indices (0-35 main, 36-39 armor boots-first, 40 offhand); the replay side
     * maps them to the ones player data uses.
     *
     * Item identity is the material id only. Enchantments and durability are not
     * recorded: they need the full component tree, and tool *tier* already
     * accounts for most of the difference in how long a block takes to break.
     */
    public static byte[] inventory(int selectedSlot, int[] slots, String[] ids,
                                   int[] counts, int n) {
        ByteWriter w = new ByteWriter();
        w.varInt(selectedSlot);
        w.varInt(n);
        for (int i = 0; i < n; i++) {
            w.varInt(slots[i]);
            w.string(ids[i]);
            w.varInt(counts[i]);
        }
        return w.toByteArray();
    }

    /** The hotbar slot (0-8) the player switched to. */
    public static byte[] heldSlot(int slot) {
        ByteWriter w = new ByteWriter();
        w.varInt(slot);
        return w.toByteArray();
    }

    /** A chat message, as typed. */
    public static byte[] chat(String message) {
        ByteWriter w = new ByteWriter();
        w.string(message == null ? "" : message);
        return w.toByteArray();
    }

    /** A dropped item: whole stack (ctrl-Q) or a single item (Q). */
    public static byte[] dropItem(boolean fullStack) {
        return new byte[] { (byte) (fullStack ? 1 : 0) };
    }

    /** An arm swing, carrying which hand swung (0 main, 1 off). */
    public static byte[] swing(int hand) {
        return new byte[] { (byte) hand };
    }

    /**
     * An absolute position the server moved the player to.
     *
     * Same encoding as the session_start marker's position, minus the string:
     * replay applies it to its view outright instead of accumulating a delta.
     */
    public static byte[] reanchor(double x, double y, double z,
                                  float yaw, float pitch, int dimensionId) {
        ByteWriter w = new ByteWriter();
        w.float64BE(x);
        w.float64BE(y);
        w.float64BE(z);
        w.float32BE(yaw);
        w.float32BE(pitch);
        w.varInt(dimensionId);
        return w.toByteArray();
    }

    /**
     * A marker carrying the exact position it was recorded at.
     *
     * The event header stores only a coarse chunk (64-block granularity, no Y),
     * which is deliberate — it keeps events small and avoids logging precise
     * player tracks. But a replay bot has to be *placed* somewhere before it
     * connects, and 64 blocks of slop is enough to put it outside interaction
     * range of everything its trace touches, or inside a wall. So session_start,
     * and only session_start, records the real position.
     *
     * The extra fields are appended after the marker string, so a reader that
     * predates them stops at the string and sees an ordinary marker.
     */
    public static byte[] markerAt(String marker, double x, double y, double z,
                                  float yaw, float pitch) {
        ByteWriter w = new ByteWriter();
        w.string(marker == null ? "" : marker);
        w.float64BE(x);
        w.float64BE(y);
        w.float64BE(z);
        w.float32BE(yaw);
        w.float32BE(pitch);
        return w.toByteArray();
    }
}
