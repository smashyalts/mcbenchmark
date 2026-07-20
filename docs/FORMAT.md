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
15 CREATIVE_SET
```

## 2. Payload encodings

| Kind | Fields |
|------|--------|
| `MOVE` | `dx dy dz yaw pitch` (5× `f32le`), `on_ground` (`bool`) |
| `SPRINT_TOGGLE` / `SNEAK_TOGGLE` | `on` (`bool`) |
| `DIG` | `action` `x` `y` `z` `face` (5× `VarInt`); action 0=start 1=cancel 2=finish |
| `PLACE_BLOCK` | `x` `y` `z` `face` `hand` (5× `VarInt`) |
| `USE_ITEM` | `hand` `item_id` (2× `VarInt`) |
| `INTERACT_ENTITY` | `target_hint` `action` (2× `VarInt`) |
| `ATTACK_ENTITY` | `target_hint` (`VarInt`) |
| `INV_OPEN` | `container_type` (`VarInt`), `has_pos` (`bool`), then if set `x` `y` `z` (3× `VarInt`) — the container's block position, used to trigger the open on replay |
| `INV_CLICK` | `window_id` `slot` `button` `click_type` (4× `VarInt`) — captured `window_id` is ignored on replay (a live id is used) |
| `INV_CLOSE` | `window_id` (`VarInt`) |
| `CMD` | `command` (`String`, includes leading `/`) |
| `MOB_SPAWN` | `entity_type` (`VarInt`), `tag` (`String`) |
| `MOB_DESPAWN` | `entity_type` `reason` (2× `VarInt`) |
| `MARKER` | `marker` (`String`) |
| `CREATIVE_SET` | `slot` `item_id` `count` (3× `VarInt`) — creative inventory set; server writes the item straight into the slot (replay/demo only) |

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
  schema_version   VarInt   1
  protocol_version VarInt
  world_profile    String
  trace_id         String
  duration_us      VarLong
  event_count      VarInt
  repeat event_count times:
    delta_offset_us VarLong  microseconds since previous event
    kind            VarInt   (same kind enum as §1)
    data_len        VarInt
    data            bytes    (same payload encodings as §2)
```

Trace files are written and read only by Go, so they use LZ4 (`pierrec/lz4`).
Reference: Go `internal/tracefile`.

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
