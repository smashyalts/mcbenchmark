# Binary Format Contract

This is the authoritative specification for every binary format crossing a
language or process boundary. The Java plugin and the Go tools each implement
it independently; the `interop-check` tool proves they agree byte-for-byte.

## 0. Primitive encodings

| Type        | Encoding |
|-------------|----------|
| `VarInt`    | Minecraft LEB128 over the two's-complement 32-bit value. Negative values always take 5 bytes. |
| `VarLong`   | 64-bit analogue of VarInt (up to 10 bytes). |
| `i64` / `i32` / `u16` | Big-endian, two's complement. |
| `f32le`     | IEEE-754 32-bit, **little-endian** — used by RawEvent payload floats. |
| `f32be` / `f64be` | IEEE-754, big-endian — used by the replay protocol layer. |
| `String`    | `VarInt` byte-length prefix, then UTF-8 bytes. |
| `bool`      | one byte, `0` or `1`. |

Reference implementations: Go `internal/mcwire`, Java `com.mcbench.capture.io.ByteWriter`.

## 1. RawEvent (plugin output, per event)

```
t_micro        i64        microseconds since plugin start
player_id      32 bytes   SHA-256(uuid_bytes || salt)
session_seq    VarInt     per-player login sequence, 0-based
dimension_id   VarInt     0=overworld 1=nether 2=end 3=other
coarse_chunk_x VarInt     floorDiv(blockX>>4, 4)
coarse_chunk_z VarInt     floorDiv(blockZ>>4, 4)
region_id      String     arena/lobby id, or "" if unset
kind           VarInt     event kind (see below)
payload_len    VarInt
payload        bytes      kind-specific, see §2
```

Reference: Go `internal/rawevent`, Java `com.mcbench.capture.model.RawEvent`.

### Event kinds

```
0 MOVE   1 SPRINT_TOGGLE  2 SNEAK_TOGGLE  3 DIG    4 PLACE_BLOCK
5 USE_ITEM  6 INTERACT_ENTITY  7 ATTACK_ENTITY  8 INV_OPEN  9 INV_CLICK
10 INV_CLOSE  11 CMD  12 MOB_SPAWN  13 MOB_DESPAWN  14 MARKER
15 CREATIVE_SET  16 REANCHOR  17 INVENTORY_SNAPSHOT
```

## 2. Payload encodings

| Kind | Fields |
|------|--------|
| `MOVE` | `dx dy dz yaw pitch` (5× `f32le`), `on_ground` (`bool`) |
| `SPRINT_TOGGLE` / `SNEAK_TOGGLE` | `on` (`bool`) |
| `DIG` | `action` `x` `y` `z` `face` (5× `VarInt`); action 0=start 1=cancel 2=finish |
| `PLACE_BLOCK` | `x` `y` `z` `face` `hand` (5× `VarInt`) — the block **clicked against** and which face of it, *not* the block that appeared; the new block lands one step along `face`, which is how `use_item_on` expresses a placement |
| `USE_ITEM` | `hand` `item_id` (2× `VarInt`) |
| `INTERACT_ENTITY` | `target_hint` `action` (2× `VarInt`) |
| `ATTACK_ENTITY` | `target_hint` (`VarInt`) |
| `INV_OPEN` | `container_type` (`VarInt`), `has_pos` (`bool`), then if set `x` `y` `z` (3× `VarInt`) — the container's block position, used to trigger the open on replay |
| `INV_CLICK` | `window_id` `slot` `button` `click_type` (4× `VarInt`) — captured `window_id` is ignored on replay (a live id is used) |
| `INV_CLOSE` | `window_id` (`VarInt`) |
| `CMD` | `command` (`String`, includes leading `/`) |
| `MOB_SPAWN` | `entity_type` (`VarInt`), `tag` (`String`) |
| `MOB_DESPAWN` | `entity_type` `reason` (2× `VarInt`) |
| `MARKER` | `marker` (`String`), then optionally `x` `y` `z` (3× `Float64BE`) and `yaw` `pitch` (2× `Float32BE`) |
| `REANCHOR` | `x` `y` `z` (3× `Float64BE`), `yaw` `pitch` (2× `Float32BE`), `dimension` (`VarInt`) — an absolute position the server moved the player to |
| `INVENTORY_SNAPSHOT` | `selected_slot` (`VarInt`), `item_count` (`VarInt`), then per item: `slot` (`VarInt`), `id` (`String`), `count` (`VarInt`) — the player's inventory at login |
| `CREATIVE_SET` | `slot` `item_id` `count` (3× `VarInt`) — creative inventory set; server writes the item straight into the slot (replay/demo only) |

Only the `session_start` marker carries the trailing position, and it is the one
exact position in the whole format — every other event locates itself with the
header's coarse chunk, which is 64 blocks wide and has no `Y`. The compiler
turns it into the trace's origin so `bench-playerdata` can place the replay bot
there before it logs in; see §4.

`REANCHOR` exists because movement is stored as a **delta** from the previous
packet, which is only meaningful while the player moves under their own power.
A teleport, a respawn or a world change arrives as one packet at the
destination. Without an anchor the capture would record a single enormous delta
— 1700 blocks in a tick for a `/tp` — and the replay bot would send it verbatim,
be rejected as an illegal move, and be rubber-banded. From there it is somewhere
the trace does not think it is, and since dig and place carry absolute
coordinates, every block event for the rest of the session misses.

The capture plugin does two things at each discontinuity, and both matter: it
emits the anchor, **and** it moves the delta baseline to the new position so the
*next* packet is not measured across the jump. Verified on a live server: a
1700-block `/tp` mid-session produced one anchor and left the largest delta in
the whole capture at 0.35 blocks.

Replay adopts an anchor only when it is close enough for the server to accept
the bot claiming it (under 8 blocks). A client cannot teleport itself, so a real
teleport is reproduced only if the benchmark server teleports the bot too — the
captured command replaying, or the same portal — which arrives as a server
`sync_position` the reader already folds into the view. The remainder is counted
as `relocations_unreproduced` in `run.json` rather than faked.

`INVENTORY_SNAPSHOT` is recorded once per login, and exists because a replay
client cannot give itself items: there is no serverbound packet for it outside
creative mode. `bench-playerdata` writes the stacks into the bot's player data
before it connects instead. Without them every bot mines barehanded, and tool
tier dominates block-break time — barehanded stone is 7.5 seconds against a
diamond pickaxe's 0.4 — so a mining trace replays as a bot swinging at blocks
that never break.

Slots are Bukkit indices: 0–35 main inventory, 36–39 armor (boots first), 40
offhand. Player data numbers them differently (0–35, 100–103, −106), and
`bench-playerdata` maps between them.

Identity is the material id alone. Enchantments and durability are **not**
recorded — they need the full item-component tree — so an Efficiency V pickaxe
replays as a plain one. Tool tier still accounts for most of the difference.

The position fields are optional so that captures written before they existed
still decode: a reader that runs out of bytes after the `marker` string has read
a plain marker, not a truncated one.

## 3. Capture log file (`raw-*.bin`)

A sequence of frames:

```
repeat:
  frame_len   u32           length of header + compressed payload
  header:
    schema_version VarInt    currently 1
    server_id      String
    start_ms       i64        epoch millis of first event in frame
    end_ms         i64        epoch millis of last event in frame
  compressed_payload         zlib (RFC 1950) of:
    repeat: [event_len VarInt][RawEvent bytes]
```

Compression is **zlib** (Java `java.util.zip.Deflater` ↔ Go `compress/zlib`),
chosen because this is the Java→Go boundary and both are standard-library
implementations. Reference: Go `internal/rawlog`, Java `CaptureLogWriter`.

## 4. Trace file (`trace-*.bin`, compiler output)

```
magic "MCT1" (4 bytes)
lz4-frame of:
  schema_version   VarInt   3
  protocol_version VarInt
  world_profile    String
  trace_id         String
  duration_us      VarLong
  has_origin       bool     (schema >= 2 only)
  if has_origin:
    x, y, z        3× Float64BE
    yaw, pitch     2× Float32BE
    dimension      VarInt   (0 overworld, 1 nether, 2 end — as §1)
    exact          bool     false if inferred rather than captured
  has_inventory    bool     (schema >= 3 only)
  if has_inventory:
    selected_slot  VarInt
    item_count     VarInt
    per item:      slot VarInt, id String, count VarInt
  event_count      VarInt
  repeat event_count times:
    delta_offset_us VarLong  microseconds since previous event
    kind            VarInt   (same kind enum as §1)
    data_len        VarInt
    data            bytes    (same payload encodings as §2)
```

Trace files are written and read only by Go, so they use LZ4 (`pierrec/lz4`).
Reference: Go `internal/tracefile`.

Older files still read: schema 1 carries no origin, schema 2 no inventory.
Schema 2 added the origin (where the captured player stood when the session
began); schema 3 the login inventory (§2, `INVENTORY_SNAPSHOT`).

The origin exists because a replay bot cannot choose its own spawn — the server
decides, and for an account that has never logged in that means world spawn.
Both consequences are silent. Dig and place carry absolute coordinates, so a bot
at spawn is out of interaction range of every block its trace touches and the
server drops those actions while the run still counts them as replayed; and a
bot left hovering because spawn is not solid ground is kicked with "Flying is not
enabled on this server" after four seconds. `bench-playerdata` writes the origin
into each bench account's player data so the bot is already in place at login.

The compiler resolves it from, in order: the `session_start` marker's position
(exact); the first block the session dug or placed, standing on top of it (also
exact — a player who broke a block was within range of it); or nothing at all.
There is deliberately no fallback to the header's coarse chunk: 64 blocks of
slop with no `Y` cannot be stood on, and a guessed `Y` buries the bot in stone or
leaves it hovering, both worse than an obviously unplaced bot at spawn.

## 5. Manifest (`manifest.json`)

```json
{
  "schema_version": 1,
  "protocol_version": 769,
  "world_profile": "bench-arena-v1",
  "run_id": "2026-07-18-2355",
  "traces": [
    { "file": "trace-000001.bin", "duration_s": 1800, "events": 7201,
      "tags": ["combat", "build"] }
  ]
}
```

Tags are derived by the compiler from each session's event mix
(`combat`, `build`, `traverse`, `commands`, or `mixed`).
