package com.mcbench.capture;

import com.mcbench.capture.model.Payloads;
import com.mcbench.capture.model.PlayerIndex;
import com.mcbench.capture.model.PlayerSession;
import com.mcbench.capture.model.RawEvent;

import java.util.UUID;

import org.bukkit.Location;
import org.bukkit.Material;
import org.bukkit.entity.Entity;
import org.bukkit.entity.Player;
import org.bukkit.inventory.ItemStack;
import org.bukkit.event.EventHandler;
import org.bukkit.event.EventPriority;
import org.bukkit.event.Listener;
import org.bukkit.event.block.Action;
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
import org.bukkit.event.player.PlayerRespawnEvent;
import org.bukkit.event.player.PlayerTeleportEvent;

/**
 * CaptureListener translates Bukkit events into RawEvents via CaptureManager.
 * Handlers use MONITOR priority and ignoreCancelled where sensible so capture
 * observes what the server actually applied without altering gameplay.
 *
 * This covers the cold event kinds only — the ones that fire a few times per
 * player per minute. Movement, the one kind whose rate scales with player count
 * and tick rate, is captured from packets instead (see
 * {@link PacketCaptureListener}) and never reaches the main thread.
 */
public final class CaptureListener implements Listener {
    private final CaptureManager mgr;
    private final PlayerIndex index;
    private final boolean captureCommands;
    private final int maxCommandLength;
    private final boolean captureInventory;

    public CaptureListener(CaptureManager mgr, PlayerIndex index, boolean captureCommands,
                           int maxCommandLength) {
        this(mgr, index, captureCommands, maxCommandLength, true);
    }

    public CaptureListener(CaptureManager mgr, PlayerIndex index, boolean captureCommands,
                           int maxCommandLength, boolean captureInventory) {
        this.mgr = mgr;
        this.index = index;
        this.captureCommands = captureCommands;
        this.maxCommandLength = maxCommandLength;
        this.captureInventory = captureInventory;
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
        index.add(p.getUniqueId(), loc.getX(), loc.getY(), loc.getZ());
        recordInventory(p, loc);
    }

    /**
     * Records what the player is carrying at login, so a replay bot can hold it.
     *
     * Tool tier dominates block-break time — barehanded stone takes 7.5 seconds
     * against a diamond pickaxe's 0.4 — so a trace recorded with a pickaxe and
     * replayed empty-handed reproduces neither the timing nor, for harder blocks,
     * the break at all. bench-playerdata writes this into the bot's player data
     * before it connects, which is the only way to arm a client: a replay client
     * cannot give itself items over the wire.
     *
     * One event per session, on the main thread. It allocates, unlike the hot
     * path, but it fires once per login rather than 20 times a second per player.
     */
    private void recordInventory(Player p, Location loc) {
        if (!captureInventory) {
            return;
        }
        ItemStack[] contents = p.getInventory().getContents();
        int[] slots = new int[contents.length];
        String[] ids = new String[contents.length];
        int[] counts = new int[contents.length];
        int n = 0;
        for (int i = 0; i < contents.length; i++) {
            ItemStack it = contents[i];
            if (it == null || it.getType() == Material.AIR || it.getAmount() <= 0) {
                continue;
            }
            slots[n] = i;
            ids[n] = it.getType().getKey().toString();
            counts[n] = it.getAmount();
            n++;
        }
        mgr.record(p.getUniqueId(), RawEvent.KIND_INVENTORY_SNAPSHOT,
                Payloads.inventory(p.getInventory().getHeldItemSlot(), slots, ids, counts, n),
                loc);
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
        reanchor(e.getPlayer(), e.getPlayer().getLocation());
        PlayerSession s = mgr.session(e.getPlayer().getUniqueId());
        if (s != null) {
            s.setDimensionId(CaptureManager.dimensionId(e.getPlayer().getWorld()));
        }
    }

    // Movement is NOT captured here. PlayerMoveEvent only fires for moves the
    // server already accepted, and only when something actually changed, so it
    // cannot see rejected movement or idle position packets — both of which are
    // real server load. PacketCaptureListener captures it from the wire
    // instead, on the connection's Netty thread.

    /**
     * Records an absolute position whenever the server relocates a player, and
     * moves the delta baseline with it.
     *
     * Movement is captured as a delta from the previous packet, which is only
     * meaningful while the player moves under their own power. A teleport
     * arrives as a single packet at the destination, so without this the capture
     * would record one enormous delta — 1600 blocks in a tick for a `/tp` — and
     * the replay bot would send it verbatim, be rejected as "moved too quickly",
     * and get rubber-banded. From that point the bot is somewhere the trace does
     * not think it is, and since dig and place carry absolute coordinates, every
     * block event for the rest of the session misses.
     *
     * Resetting the baseline is the half that matters: the event tells replay
     * where to jump to, and the baseline reset stops the *next* captured packet
     * from being a delta measured across the teleport.
     */
    private void reanchor(Player p, Location to) {
        if (to == null) {
            return;
        }
        PlayerSession s = mgr.session(p.getUniqueId());
        if (s == null) {
            return;
        }
        s.setPos(to.getX(), to.getY(), to.getZ(), to.getYaw(), to.getPitch());
        index.update(p.getUniqueId(), to.getX(), to.getY(), to.getZ());
        mgr.record(p.getUniqueId(), RawEvent.KIND_REANCHOR,
                Payloads.reanchor(to.getX(), to.getY(), to.getZ(), to.getYaw(),
                        to.getPitch(), CaptureManager.dimensionId(to.getWorld())),
                to);
    }

    /**
     * Teleports: commands, plugins, portals, ender pearls, spawn menus.
     *
     * MONITOR + ignoreCancelled means the teleport is going to happen, and
     * getTo() is where the player ends up. The event fires before the move is
     * applied, which is fine — the baseline has to be updated before the next
     * movement packet arrives, not after.
     */
    @EventHandler(priority = EventPriority.MONITOR, ignoreCancelled = true)
    public void onTeleport(PlayerTeleportEvent e) {
        reanchor(e.getPlayer(), e.getTo());
    }

    /** Respawn after death: the server picks the position outright. */
    @EventHandler(priority = EventPriority.MONITOR)
    public void onRespawn(PlayerRespawnEvent e) {
        reanchor(e.getPlayer(), e.getRespawnLocation());
    }

    // Sprint and sneak used to be captured here from Bukkit's toggle events.
    // They now come off the wire in PacketCaptureListener as entity_action,
    // together with the actions Bukkit cannot see — the elytra launch and the
    // horse actions — so these handlers are gone to avoid double-recording.

    // Digging is NOT captured here any more. BlockBreakEvent fires once the block
    // is already gone, so it could only ever produce a lone "finish" with no
    // start, no face and no duration — replay had to invent the start, and a
    // break that really took two seconds of per-tick destroy-progress work
    // collapsed into a single tick. It also never fired for a dig the player
    // began and abandoned, which costs the server real work. All of that arrives
    // intact on the packet path; see PacketCaptureListener.onAction.

    /**
     * Records a placement the way the protocol expresses it: the block that was
     * <em>clicked against</em>, plus the face of it that was clicked.
     *
     * This is not the block that appeared, and the difference is the whole event.
     * The serverbound packet is use_item_on, and the server derives where the
     * block goes from clicked position + face. Recording the placed block instead
     * — which is what this did, with the face hardcoded to "up" — asks the server
     * to place one block too high, against a position that in a pristine replay
     * world is air. You cannot place against air, so useItemOn returns PASS and
     * nothing happens at all.
     *
     * It fails exactly the way the missing dig START did: silently, with the run
     * still counting the event as replayed. Hence places_confirmed on the replay
     * side, which reports what the server actually built.
     */
    @EventHandler(priority = EventPriority.MONITOR, ignoreCancelled = true)
    public void onPlace(BlockPlaceEvent e) {
        var placed = e.getBlock();
        var against = e.getBlockAgainst();
        int hand = e.getHand() != null && e.getHand().name().equals("OFF_HAND") ? 1 : 0;
        int face = faceBetween(against, placed);
        // A replaceable target (tall grass, water) is clicked directly rather than
        // against a neighbour, and then the two blocks are the same one.
        var clicked = (against == null) ? placed : against;
        mgr.record(e.getPlayer().getUniqueId(), RawEvent.KIND_PLACE_BLOCK,
                Payloads.place(clicked.getX(), clicked.getY(), clicked.getZ(), face, hand),
                placed.getLocation());
    }

    /**
     * The block face pointing from {@code against} towards {@code placed}, in the
     * protocol's numbering: 0 down, 1 up, 2 north, 3 south, 4 west, 5 east.
     *
     * Returns up when the two are the same block or the neighbour is unknown,
     * which is what a client sends when it clicks a replaceable block directly.
     */
    private static int faceBetween(org.bukkit.block.Block against, org.bukkit.block.Block placed) {
        if (against == null) {
            return 1;
        }
        int dx = placed.getX() - against.getX();
        int dy = placed.getY() - against.getY();
        int dz = placed.getZ() - against.getZ();
        if (dy == 1) return 1;
        if (dy == -1) return 0;
        if (dz == -1) return 2;
        if (dz == 1) return 3;
        if (dx == -1) return 4;
        if (dx == 1) return 5;
        return 1; // same block: clicked a replaceable target
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
        int hand = e.getHand() != null && e.getHand().name().equals("OFF_HAND") ? 1 : 0;
        mgr.record(e.getPlayer().getUniqueId(), RawEvent.KIND_INTERACT_ENTITY,
                Payloads.entityRef(e.getRightClicked().getType().getKey().toString(), hand),
                e.getPlayer().getLocation());
    }

    @EventHandler(priority = EventPriority.MONITOR, ignoreCancelled = true)
    public void onAttack(EntityDamageByEntityEvent e) {
        if (!(e.getDamager() instanceof Player p)) {
            return;
        }
        mgr.record(p.getUniqueId(), RawEvent.KIND_ATTACK_ENTITY,
                Payloads.entityRef(e.getEntity().getType().getKey().toString(), 0),
                p.getLocation());
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
        // Allocation-free path: mob spawn rate is set by farms and spawners, not
        // by players, so this is the one main-thread kind that can arrive in
        // floods. See CaptureManager.recordMobEvent.
        mgr.recordMobEvent(near, RawEvent.KIND_MOB_SPAWN, ent.getType().ordinal(),
                e.getSpawnReason().name(), at);
    }

    @EventHandler(priority = EventPriority.MONITOR)
    public void onDeath(EntityDeathEvent e) {
        Entity ent = e.getEntity();
        Location at = ent.getLocation();
        UUID near = index.nearest(at.getX(), at.getY(), at.getZ());
        if (near == null) {
            return;
        }
        mgr.recordMobEvent(near, RawEvent.KIND_MOB_DESPAWN, ent.getType().ordinal(), null, at);
    }
}
