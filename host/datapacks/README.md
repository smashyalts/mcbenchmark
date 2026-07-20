# Benchmark datapacks

Optional, opt-in. Drop a directory into `<server>/world/datapacks/` and restart.

## no-locator-bar

Stops players transmitting locator-bar waypoints, by zeroing the
`minecraft:waypoint_transmit_range` attribute every tick.

**Why it exists.** On Paper 26.1.2, `ServerWaypointManager.updateWaypoint` is
the first thing to fall over under player load. Every accepted movement packet
walks the other players to decide who can see this player's waypoint, so the
cost grows with the square of the player count. It is what capped local runs at
550 players on a flat world and collapsed a 300-player run on a normal world to
0 TPS — both times with the capture plugin accounting for under 0.03% of the
main thread. Paper's watchdog dumps name it directly:

```
net.minecraft.world.waypoints.WaypointTransmitter.doesSourceIgnoreReceiver
net.minecraft.server.waypoints.ServerWaypointManager.updateWaypoint
net.minecraft.world.entity.Entity.setPosRaw
net.minecraft.server.network.ServerGamePacketListenerImpl.handleMovePlayer
```

There is **no `locatorBar` gamerule** on this version — the command parser
rejects the name and the string does not appear in the server jar. The attribute
is the control point.

**Read this before enabling it.** Installing this makes the benchmark *stop*
being 1:1 with production. If your production server runs 26.1.2 with the
locator bar active, then this bottleneck is real production load, and a
benchmark without it will report a headroom you do not have. Use it only to:

- separate "the server is slow" from "the plugin is slow" while profiling, or
- reach player counts your test hardware otherwise cannot, when the thing being
  measured is not the waypoint system.

For a genuine peak-load test, leave it out — and plan for the locator bar being
your ceiling.
