package com.mcbench.capture;

import com.mcbench.capture.model.Payloads;
import com.mcbench.capture.model.PlayerIndex;
import com.mcbench.capture.model.PlayerSession;
import com.mcbench.capture.model.RawEvent;

import java.util.UUID;

import org.bukkit.Location;
import org.bukkit.entity.Entity;
import org.bukkit.entity.Player;
import org.bukkit.event.EventHandler;
import org.bukkit.event.EventPriority;
import org.bukkit.event.Listener;
import org.bukkit.event.block.Action;
import org.bukkit.event.block.BlockBreakEvent;
import org.bukkit.event.block.BlockPlaceEvent;
import org.bukkit.event.entity.CreatureSpawnEvent;
import org.bukkit.event.entity.EntityDamageByEntityEvent;
import org.bukkit.event.entity.EntityDeathEvent;
import org.bukkit.event.inventory.InventoryClickEvent;
import org.bukkit.event.inventory.InventoryCloseEvent;
import org.bukkit.event.inventory.InventoryOpenEvent;
import org.bukkit.event.player.PlayerChangedWorldEvent;
import org.bukkit.event.player.PlayerCommandPreprocessEvent;
import org.bukkit.event.player.PlayerInteractEntityEvent;
import org.bukkit.event.player.PlayerInteractEvent;
import org.bukkit.event.player.PlayerJoinEvent;
import org.bukkit.event.player.PlayerQuitEvent;
import org.bukkit.event.player.PlayerToggleSneakEvent;
import org.bukkit.event.player.PlayerToggleSprintEvent;

/**
 * CaptureListener translates Bukkit events into RawEvents via CaptureManager.
 * Handlers use MONITOR priority and ignoreCancelled where sensible so capture
 * observes what the server actually applied without altering gameplay.
 *
 * This covers the cold event kinds only — the ones that fire a few times per
 * player per minute. Movement, the one kind whose rate scales with player count
 * and tick rate, is captured from packets instead (see
 * {@link PacketMovementListener}) and never reaches the main thread.
 */
public final class CaptureListener implements Listener {
    private final CaptureManager mgr;
    private final PlayerIndex index;
    private final boolean captureCommands;
    private final int maxCommandLength;

    public CaptureListener(CaptureManager mgr, PlayerIndex index, boolean captureCommands,
                           int maxCommandLength) {
        this.mgr = mgr;
        this.index = index;
        this.captureCommands = captureCommands;
        this.maxCommandLength = maxCommandLength;
    }

    @EventHandler(priority = EventPriority.MONITOR)
    public void onJoin(PlayerJoinEvent e) {
        Player p = e.getPlayer();
        Location loc = p.getLocation();
        // Order matters here. Registering the session opens the packet path, so
        // from that line on this player's Netty thread may already be capturing
        // movement. Everything the packet path reads — the delta baseline and the
        // dimension — is therefore seeded first, and the session_start marker is
        // recorded immediately after, so it does not end up timestamped behind
        // movement from its own session. The index update, which the packet path
        // does not depend on, goes last.
        mgr.onJoin(p.getUniqueId(), loc, CaptureManager.dimensionId(loc.getWorld()));
        // The exact position rides along with this marker: it is what
        // bench-playerdata uses to place the replay bot before it logs in, and
        // the coarse chunk in the event header is far too imprecise for that.
        mgr.record(p.getUniqueId(), RawEvent.KIND_MARKER,
                Payloads.markerAt("session_start", loc.getX(), loc.getY(), loc.getZ(),
                        loc.getYaw(), loc.getPitch()), loc);
        index.update(p.getUniqueId(), loc.getX(), loc.getY(), loc.getZ());
    }

    @EventHandler(priority = EventPriority.MONITOR)
    public void onQuit(PlayerQuitEvent e) {
        UUID id = e.getPlayer().getUniqueId();
        mgr.record(id, RawEvent.KIND_MARKER,
                Payloads.marker("session_end"), e.getPlayer().getLocation());
        // Retire the session BEFORE evicting from the index, not after. The
        // packet listener looks the session up first and only then touches the
        // index, so removing the session first means a late in-flight movement
        // packet returns early instead of re-inserting a departed player into the
        // index — where nothing would ever remove it again.
        mgr.onQuit(id);
        index.remove(id);
    }

    /**
     * Keeps the session's dimension current.
     *
     * A movement packet carries coordinates but not a world, so the packet path
     * reads the dimension from the session. Without this handler a player who
     * walks through a nether portal keeps producing movement stamped as
     * overworld for the rest of the session.
     */
    @EventHandler(priority = EventPriority.MONITOR)
    public void onWorldChange(PlayerChangedWorldEvent e) {
        PlayerSession s = mgr.session(e.getPlayer().getUniqueId());
        if (s != null) {
            s.setDimensionId(CaptureManager.dimensionId(e.getPlayer().getWorld()));
        }
    }

    // Movement is NOT captured here. PlayerMoveEvent only fires for moves the
    // server already accepted, and only when something actually changed, so it
    // cannot see rejected movement or idle position packets — both of which are
    // real server load. PacketMovementListener captures it from the wire
    // instead, on the connection's Netty thread.

    @EventHandler(priority = EventPriority.MONITOR, ignoreCancelled = true)
    public void onSprint(PlayerToggleSprintEvent e) {
        mgr.record(e.getPlayer().getUniqueId(), RawEvent.KIND_SPRINT_TOGGLE,
                Payloads.toggle(e.isSprinting()), e.getPlayer().getLocation());
    }

    @EventHandler(priority = EventPriority.MONITOR, ignoreCancelled = true)
    public void onSneak(PlayerToggleSneakEvent e) {
        mgr.record(e.getPlayer().getUniqueId(), RawEvent.KIND_SNEAK_TOGGLE,
                Payloads.toggle(e.isSneaking()), e.getPlayer().getLocation());
    }

    @EventHandler(priority = EventPriority.MONITOR, ignoreCancelled = true)
    public void onBreak(BlockBreakEvent e) {
        var b = e.getBlock();
        // action 2 = finish; face is unknown from this event, use 1 (up) as a
        // stable placeholder consumed by the replay client.
        mgr.record(e.getPlayer().getUniqueId(), RawEvent.KIND_DIG,
                Payloads.dig(2, b.getX(), b.getY(), b.getZ(), 1), b.getLocation());
    }

    @EventHandler(priority = EventPriority.MONITOR, ignoreCancelled = true)
    public void onPlace(BlockPlaceEvent e) {
        var b = e.getBlock();
        int hand = e.getHand() != null && e.getHand().name().equals("OFF_HAND") ? 1 : 0;
        mgr.record(e.getPlayer().getUniqueId(), RawEvent.KIND_PLACE_BLOCK,
                Payloads.place(b.getX(), b.getY(), b.getZ(), 1, hand), b.getLocation());
    }

    @EventHandler(priority = EventPriority.MONITOR, ignoreCancelled = true)
    public void onInteract(PlayerInteractEvent e) {
        if (e.getAction() != Action.RIGHT_CLICK_AIR && e.getAction() != Action.RIGHT_CLICK_BLOCK) {
            return;
        }
        if (e.getItem() == null) {
            return;
        }
        int hand = e.getHand() != null && e.getHand().name().equals("OFF_HAND") ? 1 : 0;
        int itemId = e.getItem().getType().ordinal();
        mgr.record(e.getPlayer().getUniqueId(), RawEvent.KIND_USE_ITEM,
                Payloads.useItem(hand, itemId), e.getPlayer().getLocation());
    }

    @EventHandler(priority = EventPriority.MONITOR, ignoreCancelled = true)
    public void onInteractEntity(PlayerInteractEntityEvent e) {
        int hint = e.getRightClicked().getType().ordinal();
        mgr.record(e.getPlayer().getUniqueId(), RawEvent.KIND_INTERACT_ENTITY,
                Payloads.interactEntity(hint, 0), e.getPlayer().getLocation());
    }

    @EventHandler(priority = EventPriority.MONITOR, ignoreCancelled = true)
    public void onAttack(EntityDamageByEntityEvent e) {
        if (!(e.getDamager() instanceof Player p)) {
            return;
        }
        int hint = e.getEntity().getType().ordinal();
        mgr.record(p.getUniqueId(), RawEvent.KIND_ATTACK_ENTITY,
                Payloads.attackEntity(hint), p.getLocation());
    }

    @EventHandler(priority = EventPriority.MONITOR)
    public void onInvOpen(InventoryOpenEvent e) {
        if (!(e.getPlayer() instanceof Player p)) {
            return;
        }
        int type = e.getInventory().getType().ordinal();
        // Block containers expose a world location; record it so replay can
        // trigger the open with "use item on" and read back the live window id.
        org.bukkit.Location loc = e.getInventory().getLocation();
        byte[] payload = (loc != null)
                ? Payloads.invOpen(type, loc.getBlockX(), loc.getBlockY(), loc.getBlockZ())
                : Payloads.invOpen(type);
        mgr.record(p.getUniqueId(), RawEvent.KIND_INV_OPEN, payload, p.getLocation());
    }

    @EventHandler(priority = EventPriority.MONITOR)
    public void onInvClick(InventoryClickEvent e) {
        if (!(e.getWhoClicked() instanceof Player p)) {
            return;
        }
        mgr.record(p.getUniqueId(), RawEvent.KIND_INV_CLICK,
                Payloads.invClick(0, e.getRawSlot(), e.getHotbarButton(), e.getClick().ordinal()),
                p.getLocation());
    }

    @EventHandler(priority = EventPriority.MONITOR)
    public void onInvClose(InventoryCloseEvent e) {
        if (!(e.getPlayer() instanceof Player p)) {
            return;
        }
        mgr.record(p.getUniqueId(), RawEvent.KIND_INV_CLOSE,
                Payloads.invClose(0), p.getLocation());
    }

    @EventHandler(priority = EventPriority.MONITOR, ignoreCancelled = true)
    public void onCommand(PlayerCommandPreprocessEvent e) {
        if (!captureCommands) {
            return;
        }
        String msg = e.getMessage();
        if (msg.length() > maxCommandLength) {
            msg = msg.substring(0, maxCommandLength);
        }
        mgr.record(e.getPlayer().getUniqueId(), RawEvent.KIND_CMD,
                Payloads.command(msg), e.getPlayer().getLocation());
    }

    @EventHandler(priority = EventPriority.MONITOR, ignoreCancelled = true)
    public void onSpawn(CreatureSpawnEvent e) {
        // Mob-context events are not tied to a player; attribute to the nearest
        // player within 48 blocks so they land in that session's stream.
        Entity ent = e.getEntity();
        Location at = ent.getLocation();
        UUID near = index.nearest(at.getX(), at.getY(), at.getZ());
        if (near == null) {
            return;
        }
        mgr.record(near, RawEvent.KIND_MOB_SPAWN,
                Payloads.mobSpawn(ent.getType().ordinal(), e.getSpawnReason().name()),
                at);
    }

    @EventHandler(priority = EventPriority.MONITOR)
    public void onDeath(EntityDeathEvent e) {
        Entity ent = e.getEntity();
        Location at = ent.getLocation();
        UUID near = index.nearest(at.getX(), at.getY(), at.getZ());
        if (near == null) {
            return;
        }
        mgr.record(near, RawEvent.KIND_MOB_DESPAWN,
                Payloads.mobDespawn(ent.getType().ordinal(), 0), at);
    }
}
