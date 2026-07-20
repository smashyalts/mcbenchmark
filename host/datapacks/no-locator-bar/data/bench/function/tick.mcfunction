# Zero every player's waypoint transmit range.
#
# There is no locatorBar gamerule on Paper 26.1.2 -- the command parser rejects
# the name and the string does not appear in the server jar. The locator bar is
# driven by the minecraft:waypoint_transmit_range attribute, and a player who
# transmits nothing costs nothing in ServerWaypointManager.updateWaypoint.
#
# Re-applied every tick because the attribute resets on respawn and dimension
# change, and because players join continuously during a ramp. Setting an
# attribute that already holds the value is cheap; scanning @a is not free, but
# it is O(players) once per tick against the O(players^2) work it removes.
attribute @a minecraft:waypoint_transmit_range base set 0
