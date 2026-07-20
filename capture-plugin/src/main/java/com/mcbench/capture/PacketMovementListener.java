package com.mcbench.capture;

import com.mcbench.capture.model.PlayerIndex;
import com.mcbench.capture.model.PlayerSession;

import com.github.retrooper.packetevents.event.PacketListenerAbstract;
import com.github.retrooper.packetevents.event.PacketListenerPriority;
import com.github.retrooper.packetevents.event.PacketReceiveEvent;
import com.github.retrooper.packetevents.protocol.packettype.PacketType;
import com.github.retrooper.packetevents.protocol.world.Location;
import com.github.retrooper.packetevents.wrapper.play.client.WrapperPlayClientPlayerFlying;

import java.util.UUID;

/**
 * Captures movement from the client's own packets instead of PlayerMoveEvent.
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
 * movement capture no longer touches the server main thread at all. The
 * per-player ring stays single-producer because Netty pins a channel to one
 * event loop for its lifetime — the producer is simply a different thread per
 * session rather than one thread for all of them. {@link PlayerSession} binds
 * that thread on first write and refuses (and counts) anything else, so if the
 * assumption is ever wrong it shows up as {@code OFF-THREAD=} in the stats
 * rather than as silent corruption.
 *
 * The listener never cancels or modifies a packet; it only reads.
 */
public final class PacketMovementListener extends PacketListenerAbstract {
    private final CaptureManager mgr;
    private final PlayerIndex index;

    public PacketMovementListener(CaptureManager mgr, PlayerIndex index) {
        // MONITOR: observe what other plugins have already decided, and never
        // sit in front of them on the packet path.
        super(PacketListenerPriority.MONITOR);
        this.mgr = mgr;
        this.index = index;
    }

    @Override
    public void onPacketReceive(PacketReceiveEvent event) {
        if (!WrapperPlayClientPlayerFlying.isFlying(event.getPacketType())) {
            return;
        }
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
                || type == PacketType.Play.Client.PLAYER_FLYING;
    }
}
