package com.mcbench.capture.model;

import com.mcbench.capture.io.ByteWriter;

/**
 * RawEvent is one captured event. Its wire layout is identical to the Go
 * {@code rawevent.RawEvent} (docs/FORMAT.md section 2.1):
 *
 * <pre>
 *   t_micro        int64 BE
 *   player_id      32 bytes
 *   session_seq    VarInt
 *   dimension_id   VarInt
 *   coarse_chunk_x VarInt
 *   coarse_chunk_z VarInt
 *   region_id      String (VarInt len + UTF-8)
 *   kind           VarInt
 *   payload        VarInt len + bytes
 * </pre>
 */
public final class RawEvent {
    // Event kinds, shared with the Go side.
    public static final int KIND_MOVE = 0;
    public static final int KIND_SPRINT_TOGGLE = 1;
    public static final int KIND_SNEAK_TOGGLE = 2;
    public static final int KIND_DIG = 3;
    public static final int KIND_PLACE_BLOCK = 4;
    public static final int KIND_USE_ITEM = 5;
    public static final int KIND_INTERACT_ENTITY = 6;
    public static final int KIND_ATTACK_ENTITY = 7;
    public static final int KIND_INV_OPEN = 8;
    public static final int KIND_INV_CLICK = 9;
    public static final int KIND_INV_CLOSE = 10;
    public static final int KIND_CMD = 11;
    public static final int KIND_MOB_SPAWN = 12;
    public static final int KIND_MOB_DESPAWN = 13;
    public static final int KIND_MARKER = 14;
    public static final int KIND_CREATIVE_SET = 15;
    /**
     * An absolute position the server put the player at, breaking the delta
     * chain: teleport, respawn, or world change.
     *
     * Movement is stored as deltas, which only stays correct while the player
     * moves continuously. The moment the server relocates them, every later
     * delta is measured from the new spot but replayed from the old one, so the
     * bot's absolute position is wrong for the rest of the session — and since
     * dig and place carry absolute coordinates, every block event after the
     * teleport lands somewhere the bot is not.
     */
    public static final int KIND_REANCHOR = 16;
    /**
     * The player's inventory as it stood at login, so a replay bot can hold what
     * they held.
     *
     * Without it every bot mines barehanded, and tool tier dominates block-break
     * time: barehanded stone is 7.5 seconds against a diamond pickaxe's 0.4. A
     * trace recorded with a pickaxe then replays as a bot swinging at stone that
     * never breaks, which is not the load the capture described.
     */
    public static final int KIND_INVENTORY_SNAPSHOT = 17;

    /**
     * A hotbar slot change. The login inventory says what the player carried;
     * this says what they were holding at any given moment, which is what
     * actually decides how long a block takes to break — 7.5 seconds barehanded
     * against a diamond pickaxe's 0.4 on the same stone.
     */
    public static final int KIND_HELD_SLOT = 18;

    /**
     * A chat message. One of the few player actions whose server cost scales
     * with the population rather than the sender: the message is fanned out to
     * everyone who can see it.
     */
    public static final int KIND_CHAT = 19;

    /** A Q / ctrl-Q drop, which spawns an item entity that then ticks. */
    public static final int KIND_DROP_ITEM = 20;

    /** The offhand swap (F). */
    public static final int KIND_SWAP_HANDS = 21;

    public long tMicro;
    /** Wall-clock epoch millis, used for frame headers only; NOT encoded. */
    public long epochMs;
    public byte[] playerId; // exactly 32 bytes
    public int sessionSeq;
    public int dimensionId;
    public int coarseChunkX;
    public int coarseChunkZ;
    public String regionId = "";
    public int kind;
    public byte[] payload = new byte[0];

    /** Encodes this event (without the outer per-event length prefix) into w. */
    public void encode(ByteWriter w) {
        w.int64BE(tMicro);
        w.raw(playerId);
        w.varInt(sessionSeq);
        w.varInt(dimensionId);
        w.varInt(coarseChunkX);
        w.varInt(coarseChunkZ);
        w.string(regionId == null ? "" : regionId);
        w.varInt(kind);
        w.varInt(payload.length);
        w.raw(payload);
    }
}
