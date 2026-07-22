package com.mcbench.capture;

import com.mcbench.capture.model.Payloads;
import com.mcbench.capture.model.PlayerIndex;
import com.mcbench.capture.model.PlayerSession;
import com.mcbench.capture.model.RawEvent;

import com.github.retrooper.packetevents.event.PacketListenerAbstract;
import com.github.retrooper.packetevents.event.PacketListenerPriority;
import com.github.retrooper.packetevents.event.PacketReceiveEvent;
import com.github.retrooper.packetevents.protocol.packettype.PacketType;
import com.github.retrooper.packetevents.protocol.packettype.PacketTypeCommon;
import com.github.retrooper.packetevents.protocol.world.Location;
import com.github.retrooper.packetevents.util.Vector3i;
import com.github.retrooper.packetevents.wrapper.play.client.WrapperPlayClientAnimation;
import com.github.retrooper.packetevents.wrapper.play.client.WrapperPlayClientChatMessage;
import com.github.retrooper.packetevents.wrapper.play.client.WrapperPlayClientHeldItemChange;
import com.github.retrooper.packetevents.wrapper.play.client.WrapperPlayClientPlayerDigging;
import com.github.retrooper.packetevents.wrapper.play.client.WrapperPlayClientPlayerFlying;

import java.util.UUID;

/**
 * Captures what the client tells the server about itself, from the wire.
 *
 * That is movement above all, but also the actions carried by packets a Bukkit
 * event can only describe after the fact: digging (start, abort and finish, at
 * their real times), dropping an item, swapping hands, switching hotbar slot,
 * and chat. See {@link #onAction}.
 *
 * <h3>Why packets</h3>
 * {@code PlayerMoveEvent} is a filtered, post-validation view: it fires on the
 * main thread only after the server has accepted a move, and only when the
 * position or rotation actually changed. Three things a load benchmark cares
 * about are invisible to it:
 *
 * <ul>
 *   <li><b>Rejected movement.</b> The server pays full cost to receive, decode
 *       and validate a move it then refuses ("moved too quickly"). Event-based
 *       capture records none of that work, so a replay built from it
 *       under-reproduces real load.</li>
 *   <li><b>Idle position updates.</b> A stationary client still sends periodic
 *       flying packets. No event fires, so the packet rate — the thing that
 *       actually loads the network and packet-handling path — is understated.</li>
 *   <li><b>Sub-tick timing.</b> Events collapse onto tick boundaries by
 *       construction; packets carry their real arrival time.</li>
 * </ul>
 *
 * <h3>Threading</h3>
 * PacketEvents dispatches this on the connection's Netty event-loop thread, so
 * none of this capture touches the server main thread at all. The
 * per-player ring stays single-producer because Netty pins a channel to one
 * event loop for its lifetime — the producer is simply a different thread per
 * session rather than one thread for all of them. {@link PlayerSession} binds
 * that thread on first write and refuses (and counts) anything else, so if the
 * assumption is ever wrong it shows up as {@code OFF-THREAD=} in the stats
 * rather than as silent corruption.
 *
 * The listener never cancels or modifies a packet; it only reads.
 */
public final class PacketCaptureListener extends PacketListenerAbstract {
    private final CaptureManager mgr;
    private final PlayerIndex index;

    public PacketCaptureListener(CaptureManager mgr, PlayerIndex index) {
        // MONITOR: observe what other plugins have already decided, and never
        // sit in front of them on the packet path.
        super(PacketListenerPriority.MONITOR);
        this.mgr = mgr;
        this.index = index;
    }

    @Override
    public void onPacketReceive(PacketReceiveEvent event) {
        // Cancelled by another plugin means the server will not process it, so
        // it should not appear in a trace meant to reproduce server load.
        if (event.isCancelled()) {
            return;
        }
        UUID uuid = event.getUser() == null ? null : event.getUser().getUUID();
        if (uuid == null) {
            return; // pre-login packet, nothing to attribute it to yet
        }
        PlayerSession session = mgr.session(uuid);
        if (session == null) {
            return; // join not seen yet
        }
        if (WrapperPlayClientPlayerFlying.isFlying(event.getPacketType())) {
            onMovement(event, uuid, session);
        } else {
            onAction(event, uuid, session);
        }
    }

    /**
     * The actions a client reports about itself, taken from the wire rather than
     * from Bukkit events.
     *
     * Every one of these used to be reconstructed from a Bukkit event that fires
     * after the fact, and each reconstruction lost something. BlockBreakEvent
     * fires once the block is already gone, so a dig arrived as a lone "finish"
     * with no start and no duration — replay had to invent a start, and the whole
     * multi-tick break, during which the server ticks destroy progress every
     * tick, collapsed into one tick. Digs a player began and abandoned produced
     * no event at all, though the server did the work. Dropping an item and
     * swapping hands were not captured at all.
     *
     * They are all the same packet. Taking it verbatim means the trace holds what
     * the client actually sent, when it sent it, and replay forwards it without
     * reinterpretation.
     */
    private void onAction(PacketReceiveEvent event, UUID uuid, PlayerSession session) {
        PacketTypeCommon type = event.getPacketType();
        if (type == PacketType.Play.Client.PLAYER_DIGGING) {
            WrapperPlayClientPlayerDigging w = new WrapperPlayClientPlayerDigging(event);
            Vector3i pos = w.getBlockPosition();
            int action = w.getAction().getId();
            switch (action) {
                case 0: // start
                case 1: // cancel
                case 2: // finish
                    record(uuid, session, RawEvent.KIND_DIG,
                            Payloads.dig(action, pos.getX(), pos.getY(), pos.getZ(),
                                    w.getBlockFaceId()),
                            pos.getX(), pos.getZ());
                    break;
                case 3: // drop stack
                case 4: // drop one
                    record(uuid, session, RawEvent.KIND_DROP_ITEM,
                            Payloads.dropItem(action == 3), blockX(session), blockZ(session));
                    break;
                case 6: // swap with offhand
                    record(uuid, session, RawEvent.KIND_SWAP_HANDS, EMPTY,
                            blockX(session), blockZ(session));
                    break;
                default: // release-use and stab carry no replay analogue yet
                    break;
            }
            return;
        }
        if (type == PacketType.Play.Client.ANIMATION) {
            // One swing per left-click: dig start, attack, or a miss. getHand()
            // is 0 (main) or 1 (off); getId() maps straight to the protocol value.
            int hand = new WrapperPlayClientAnimation(event).getHand().getId();
            record(uuid, session, RawEvent.KIND_SWING, Payloads.swing(hand),
                    blockX(session), blockZ(session));
            return;
        }
        if (type == PacketType.Play.Client.HELD_ITEM_CHANGE) {
            int slot = new WrapperPlayClientHeldItemChange(event).getSlot();
            if (slot >= 0 && slot <= 8) {
                record(uuid, session, RawEvent.KIND_HELD_SLOT, Payloads.heldSlot(slot),
                        blockX(session), blockZ(session));
            }
            return;
        }
        if (type == PacketType.Play.Client.CHAT_MESSAGE) {
            // Chat is captured here rather than from AsyncChatEvent for a
            // practical reason: Paper fires chat asynchronously, on a thread that
            // is neither the main thread nor this session's Netty thread, and a
            // ring has exactly one producer. On the packet path it lands on the
            // same thread as this session's movement, which already owns the ring.
            String msg = new WrapperPlayClientChatMessage(event).getMessage();
            if (msg != null && !msg.isEmpty()) {
                if (msg.length() > MAX_CHAT) {
                    msg = msg.substring(0, MAX_CHAT);
                }
                record(uuid, session, RawEvent.KIND_CHAT, Payloads.chat(msg),
                        blockX(session), blockZ(session));
            }
        }
    }

    private void record(UUID uuid, PlayerSession s, int kind, byte[] payload, int bx, int bz) {
        mgr.recordFromPacket(uuid, kind, payload, s.dimensionId(), bx, bz);
    }

    private static int blockX(PlayerSession s) { return (int) Math.floor(s.lastX()); }

    private static int blockZ(PlayerSession s) { return (int) Math.floor(s.lastZ()); }

    /** Vanilla caps a chat message at 256 characters; the server rejects longer. */
    private static final int MAX_CHAT = 256;

    private static final byte[] EMPTY = new byte[0];

    private void onMovement(PacketReceiveEvent event, UUID uuid, PlayerSession session) {
        WrapperPlayClientPlayerFlying wrapper = wrapperFor(event);
        Location loc = wrapper.getLocation();
        boolean posChanged = wrapper.hasPositionChanged();
        boolean rotChanged = wrapper.hasRotationChanged();

        double x = posChanged ? loc.getX() : session.lastX();
        double y = posChanged ? loc.getY() : session.lastY();
        double z = posChanged ? loc.getZ() : session.lastZ();
        float yaw = rotChanged ? loc.getYaw() : session.lastYaw();
        float pitch = rotChanged ? loc.getPitch() : session.lastPitch();

        if (!session.havePos()) {
            // First packet only establishes the baseline; emitting a delta from
            // an unknown previous position would invent a teleport.
            session.setPos(x, y, z, yaw, pitch);
            index.update(uuid, x, y, z);
            return;
        }

        float dx = (float) (x - session.lastX());
        float dy = (float) (y - session.lastY());
        float dz = (float) (z - session.lastZ());
        session.setPos(x, y, z, yaw, pitch);

        if (posChanged) {
            index.update(uuid, x, y, z);
        }

        // Status-only packets (neither flag set) are recorded too, with zero
        // deltas: they are real inbound packets the server must process, and
        // reproducing that rate is the point of capturing at this level.
        mgr.recordMovePacket(uuid, dx, dy, dz, yaw, pitch, wrapper.isOnGround(),
                (int) Math.floor(x), (int) Math.floor(z));
    }

    /**
     * Reuses the wrapper another listener already decoded, if there is one.
     *
     * This runs at MONITOR priority, so on a real server anything that inspects
     * movement — anticheat above all — has usually decoded the packet already.
     * PacketEvents keeps that wrapper on the event, so taking it saves both the
     * allocation and, more importantly, a second decode of the same bytes. On a
     * server where we are the only movement listener the wrapper is absent and we
     * build one, which is the cost we would have paid unconditionally before.
     *
     * The instanceof is not paranoia about the wrong packet — lastUsedWrapper
     * belongs to this event — but about a future PacketEvents that populates it
     * with some other view of a flying packet.
     */
    private static WrapperPlayClientPlayerFlying wrapperFor(PacketReceiveEvent event) {
        if (event.getLastUsedWrapper() instanceof WrapperPlayClientPlayerFlying existing) {
            return existing;
        }
        return new WrapperPlayClientPlayerFlying(event);
    }

    /** Packet types this listener consumes, for documentation and tests. */
    public static boolean handles(Object type) {
        return type == PacketType.Play.Client.PLAYER_POSITION
                || type == PacketType.Play.Client.PLAYER_POSITION_AND_ROTATION
                || type == PacketType.Play.Client.PLAYER_ROTATION
                || type == PacketType.Play.Client.PLAYER_FLYING
                || type == PacketType.Play.Client.PLAYER_DIGGING
                || type == PacketType.Play.Client.ANIMATION
                || type == PacketType.Play.Client.HELD_ITEM_CHANGE
                || type == PacketType.Play.Client.CHAT_MESSAGE;
    }
}
