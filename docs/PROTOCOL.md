# Replay Protocol Coverage

The replay client (`mc-replay`) speaks the Minecraft Java protocol directly.
Packet IDs target **protocol 775 (Minecraft 26.1.2)** and are defined in one
place: `go/internal/mcproto/ids.go`. They were extracted from the server's own
data generator, which is authoritative for any version:

```
java -DbundlerMainClass=net.minecraft.data.Main -jar cache/mojang_<ver>.jar --reports
# → generated/reports/packets.json  (packet ids by state + direction)
# → generated/reports/registries.json  (item/entity ids)
```

Retargeting a different version means regenerating that report, updating
`ids.go`, and verifying the payload shapes of the packets the client builds
(login/config IDs are stable across versions; play-phase IDs shift frequently).
This has been validated end-to-end against a real Paper 26.1.2 server — see the
"Validation" section in the top-level README.

## Connection lifecycle

```
TCP dial
  → Handshake (intent=login)
  → Login Start (offline: username + offline UUID)         [uncompressed]
  ← Set Compression        → codec switches to zlib framing
  ← Login Success          → Login Acknowledged
  == Configuration ==
  ← (registry data, tags, feature flags — ignored)
  ← Known Packs            → Client Information + empty Known Packs
  ← Keep Alive / Ping      → echo
  ← Finish Configuration   → Finish Config Ack
  == Play ==
  ← Synchronize Position   → Teleport Confirm + Position/Look   (session is "ready")
  ← Keep Alive / Ping      → echo
  ← Chunk Batch Finished   → Chunk Batch Received
  ← Start Configuration    → Config Ack (re-enters configuration)
  ← Disconnect             → session ends
```

The offline UUID is `nameUUIDFromBytes("OfflinePlayer:" + name)` (MD5, version
3), matching the vanilla server. The benchmark server **must** be in
offline-mode; if it sends an Encryption Request the client fails the session with
a clear error.

## Trace event → packet mapping

| Trace kind | Serverbound packet(s) |
|------------|----------------------|
| `MOVE` | Position+Look (absolute position accumulated from deltas) |
| `SPRINT_TOGGLE` | Entity Action start/stop sprinting |
| `SNEAK_TOGGLE` | Entity Action start/stop sneaking |
| `DIG` | Player Action (block dig) + Arm Animation |
| `PLACE_BLOCK` | Use Item On (block place) |
| `USE_ITEM` | Use Item |
| `ATTACK_ENTITY` / `INTERACT_ENTITY` | Arm Animation (swing) |
| `CMD` | Chat Command (unsigned), leading `/` stripped |
| `INV_OPEN` | Use Item On at the captured container position (server replies with `open_screen`) |
| `INV_CLICK` | Container Click, using the **live** window/state id |
| `INV_CLOSE` | Container Close (live window id) |
| `CREATIVE_SET` | Set Creative Slot (creative demos) |
| `MARKER`, `MOB_*` | counted as *skipped* (no serverbound analogue) |

### Inventory: live window & state IDs

Window ids are assigned by the server per container open, so the *captured* id is
useless on another server. Instead the client tracks the ids the server hands it
at replay time — exactly like teleport ids:

- clientbound `open_screen` → the **live window id** (`curWindow`);
- clientbound `container_set_content` / `set_slot` → the **state id** the click
  packet must echo (`curState`);
- clientbound `container_close` → reset to window 0 (the always-open player
  inventory).

`INV_CLICK` then sends a Container Click with those live ids, the captured
slot/button/mode, an empty changed-slots array, and an empty cursor. The server
**processes the click** (the point of a plugin/container benchmark) and resyncs
via Set Slot on any mismatch. Full item-movement fidelity would additionally
require modeling container contents client-side; for load testing the processed
click is what matters.

### Other deliberate limitations

- **Entity targeting** — captured `target_hint`s are semantic (e.g. "nearest
  hostile"), not live entity IDs, so combat replays the *observable* client
  behavior (arm swing) rather than a synthetic attack on a specific entity.
- **Movement anti-cheat** — real captured survival movement replays faithfully.
  Synthetic *airborne* travel (creative flight) is throttled by the server's
  flight speed check, so `gen-demo --tp` uses operator teleport commands when it
  needs to force long-distance chunk generation.
- **Block sequences** — dig/place/use carry a monotonically increasing sequence
  number per session, as the protocol requires for block-change acks.

Events with no serverbound analogue are counted in the run report
(`events_skipped`) so coverage is never silently overstated.

## Position handling

The client maintains a minimal `WorldView` (position, rotation, last teleport
id). Server teleports (Synchronize Position) may be absolute or relative per the
flags byte; both are applied correctly before the Teleport Confirm is returned.
Movement events accumulate deltas onto this position so the server continues to
accept them.
