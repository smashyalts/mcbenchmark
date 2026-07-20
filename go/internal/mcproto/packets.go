package mcproto

import (
	"crypto/md5"

	"mcbench/internal/mcwire"
)

// OfflineUUID computes the offline-mode UUID for a username, matching
// Java's UUID.nameUUIDFromBytes("OfflinePlayer:" + name) (MD5, version 3).
func OfflineUUID(name string) [16]byte {
	sum := md5.Sum([]byte("OfflinePlayer:" + name))
	sum[6] = sum[6]&0x0F | 0x30 // version 3
	sum[8] = sum[8]&0x3F | 0x80 // IETF variant
	return sum
}

// Handshake builds the serverbound handshake body.
func Handshake(protocol int32, host string, port uint16, intent int32) []byte {
	w := mcwire.NewWriter()
	w.VarInt(protocol)
	w.String(host)
	w.Uint16BE(port)
	w.VarInt(intent)
	return w.Bytes()
}

// LoginStart builds the serverbound login_start body.
func LoginStart(name string, uuid [16]byte) []byte {
	w := mcwire.NewWriter()
	w.String(name)
	w.Raw(uuid[:])
	return w.Bytes()
}

// ClientInformation builds the configuration "settings" body (protocol 775:
// includes trailing particle-status VarInt).
func ClientInformation() []byte {
	w := mcwire.NewWriter()
	w.String("en_us") // locale
	w.Byte(8)         // view distance
	w.VarInt(0)       // chat mode: enabled
	w.Bool(true)      // chat colors
	w.Byte(0x7F)      // displayed skin parts
	w.VarInt(1)       // main hand: right
	w.Bool(false)     // text filtering
	w.Bool(true)      // allow server listings
	w.VarInt(0)       // particle status: all
	return w.Bytes()
}

// KnownPacksEmpty responds to select_known_packs with zero packs, which makes
// the server send registry data inline (always valid).
func KnownPacksEmpty() []byte {
	w := mcwire.NewWriter()
	w.VarInt(0)
	return w.Bytes()
}

// KeepAlive builds a keep-alive body (same for config and play).
func KeepAlive(id int64) []byte {
	w := mcwire.NewWriter()
	w.Int64BE(id)
	return w.Bytes()
}

// Pong builds a pong body for the ping packet (int payload).
func Pong(id int32) []byte {
	w := mcwire.NewWriter()
	w.Int32BE(id)
	return w.Bytes()
}

// TeleportConfirm builds the teleport_confirm body.
func TeleportConfirm(teleportID int32) []byte {
	w := mcwire.NewWriter()
	w.VarInt(teleportID)
	return w.Bytes()
}

// ChunkBatchReceived acknowledges a chunk batch with the desired
// chunks-per-tick rate.
func ChunkBatchReceived(chunksPerTick float32) []byte {
	w := mcwire.NewWriter()
	w.Float32BE(chunksPerTick)
	return w.Bytes()
}

// PositionLook builds the serverbound position_look body.
// Protocol 775 movement flags: bit0 = on ground, bit1 = pushing against wall.
func PositionLook(x, y, z float64, yaw, pitch float32, onGround bool) []byte {
	w := mcwire.NewWriter()
	w.Float64BE(x)
	w.Float64BE(y)
	w.Float64BE(z)
	w.Float32BE(yaw)
	w.Float32BE(pitch)
	if onGround {
		w.Byte(1)
	} else {
		w.Byte(0)
	}
	return w.Bytes()
}

// Flying builds the serverbound move_player_status_only body: the movement
// flags byte and nothing else.
//
// This is what a real client sends on a tick where it has not moved or turned.
// It is not an optional nicety — a vanilla client sends exactly one movement
// packet every tick, forever, and the server pays receive/decode/validate cost
// for each one. A load generator that only sends movement when the player
// actually moves understates the packet rate several times over, and understates
// it precisely on the path this benchmark exists to measure.
func Flying(onGround bool) []byte {
	w := mcwire.NewWriter()
	if onGround {
		w.Byte(1)
	} else {
		w.Byte(0)
	}
	return w.Bytes()
}

// EntityAction builds the serverbound entity_action body. The vanilla server
// ignores the entity ID field and applies the action to the sending player.
func EntityAction(action int32) []byte {
	w := mcwire.NewWriter()
	w.VarInt(0) // entity id (ignored by server)
	w.VarInt(action)
	w.VarInt(0) // jump boost
	return w.Bytes()
}

// blockPos packs block coordinates into the protocol's position long.
func blockPos(x, y, z int32) int64 {
	return (int64(x)&0x3FFFFFF)<<38 | (int64(z)&0x3FFFFFF)<<12 | int64(y)&0xFFF
}

// BlockDig builds the serverbound block_dig ("player action") body.
func BlockDig(status, x, y, z, face, sequence int32) []byte {
	w := mcwire.NewWriter()
	w.VarInt(status)
	w.Int64BE(blockPos(x, y, z))
	w.Byte(byte(face))
	w.VarInt(sequence)
	return w.Bytes()
}

// BlockPlace builds the serverbound block_place ("use item on") body.
func BlockPlace(hand, x, y, z, face, sequence int32) []byte {
	w := mcwire.NewWriter()
	w.VarInt(hand)
	w.Int64BE(blockPos(x, y, z))
	w.VarInt(face)
	w.Float32BE(0.5) // cursor x
	w.Float32BE(0.5) // cursor y
	w.Float32BE(0.5) // cursor z
	w.Bool(false)    // inside block
	w.Bool(false)    // world border hit
	w.VarInt(sequence)
	return w.Bytes()
}

// UseItem builds the serverbound use_item body (protocol 775 includes the
// player rotation).
func UseItem(hand, sequence int32, yaw, pitch float32) []byte {
	w := mcwire.NewWriter()
	w.VarInt(hand)
	w.VarInt(sequence)
	w.Float32BE(yaw)
	w.Float32BE(pitch)
	return w.Bytes()
}

// ArmAnimation builds the serverbound swing-arm body.
func ArmAnimation(hand int32) []byte {
	w := mcwire.NewWriter()
	w.VarInt(hand)
	return w.Bytes()
}

// ChatCommand builds the serverbound (unsigned) chat_command body.
func ChatCommand(command string) []byte {
	w := mcwire.NewWriter()
	w.String(command)
	return w.Bytes()
}

// ParseSyncPosition decodes the clientbound "position" (synchronize player
// position) packet for protocol 775:
//
//	teleport_id VarInt, x/y/z f64, vel_x/y/z f64, yaw/pitch f32, flags i32
type SyncPosition struct {
	TeleportID int32
	X, Y, Z    float64
	Yaw, Pitch float32
	Flags      int32
}

// Relative-teleport flag bits.
const (
	FlagRelX     = 1 << 0
	FlagRelY     = 1 << 1
	FlagRelZ     = 1 << 2
	FlagRelYaw   = 1 << 3
	FlagRelPitch = 1 << 4
)

func ParseSyncPosition(body []byte) (SyncPosition, error) {
	r := mcwire.NewReader(body)
	var p SyncPosition
	var err error
	if p.TeleportID, err = r.VarInt(); err != nil {
		return p, err
	}
	if p.X, err = r.Float64BE(); err != nil {
		return p, err
	}
	if p.Y, err = r.Float64BE(); err != nil {
		return p, err
	}
	if p.Z, err = r.Float64BE(); err != nil {
		return p, err
	}
	for i := 0; i < 3; i++ { // velocity, unused
		if _, err = r.Float64BE(); err != nil {
			return p, err
		}
	}
	if p.Yaw, err = r.Float32BE(); err != nil {
		return p, err
	}
	if p.Pitch, err = r.Float32BE(); err != nil {
		return p, err
	}
	p.Flags, err = r.Int32BE()
	return p, err
}

// --- Inventory / container packets ---
//
// Window and state IDs are assigned by the server at replay time, exactly like
// teleport IDs: the client never reuses captured IDs. Open Screen carries the
// live window id; Container Set Content / Set Slot carry the state id the click
// packet must echo. See docs/PROTOCOL.md.

// OpenScreen decodes the clientbound open_screen packet: window_id, type, title.
type OpenScreen struct {
	WindowID int32
	Type     int32
}

func ParseOpenScreen(body []byte) (OpenScreen, error) {
	r := mcwire.NewReader(body)
	var o OpenScreen
	var err error
	if o.WindowID, err = r.VarInt(); err != nil {
		return o, err
	}
	o.Type, err = r.VarInt()
	return o, err // title (text component) follows but is unused
}

// ParseContainerStateID extracts (window_id, state_id) from the head of a
// container_set_content or container_set_slot packet — both begin with those
// two VarInts in protocol 775.
func ParseContainerStateID(body []byte) (windowID, stateID int32, err error) {
	r := mcwire.NewReader(body)
	if windowID, err = r.VarInt(); err != nil {
		return
	}
	stateID, err = r.VarInt()
	return
}

// ParseContainerClose extracts the window id from a clientbound container_close.
func ParseContainerClose(body []byte) (int32, error) {
	return mcwire.NewReader(body).VarInt()
}

// ContainerClick builds a serverbound container_click. It sends an empty
// changed-slots array and an empty carried item: the server still processes the
// click (exercising container-menu logic for the benchmark) and, on any
// mismatch, resyncs the client via Set Slot. state_id is the latest value the
// server sent for this window.
func ContainerClick(windowID, stateID, slot, button, mode int32) []byte {
	w := mcwire.NewWriter()
	w.VarInt(windowID)
	w.VarInt(stateID)
	w.Uint16BE(uint16(int16(slot))) // slot: short
	w.Byte(byte(button))
	w.VarInt(mode)
	w.VarInt(0)  // changed_slots: empty array
	w.Byte(0x00) // carried_item: empty Slot (item count VarInt 0)
	return w.Bytes()
}

// ContainerClose builds a serverbound container_close for a window id.
func ContainerClose(windowID int32) []byte {
	w := mcwire.NewWriter()
	w.VarInt(windowID)
	return w.Bytes()
}

// Abilities builds a serverbound player_abilities body. In creative (or when the
// server granted flight), setting the flying bit makes the server accept airborne
// movement, which is how the demo travels far enough to force chunk generation.
func Abilities(flying bool) []byte {
	w := mcwire.NewWriter()
	if flying {
		w.Byte(0x02) // bit 1 = is flying
	} else {
		w.Byte(0x00)
	}
	return w.Bytes()
}

// SetCreativeSlot builds a serverbound set_creative_mode_slot. Only honored when
// the player is in creative mode; the server writes the item straight into the
// inventory slot (and persists it to the player's NBT). The item is encoded as a
// 1.20.5+ Slot: count, item id, then zero added/removed components.
func SetCreativeSlot(slot, itemID, count int32) []byte {
	w := mcwire.NewWriter()
	w.Uint16BE(uint16(int16(slot))) // slot: short
	if count <= 0 {
		w.VarInt(0) // empty slot
		return w.Bytes()
	}
	w.VarInt(count)  // item count
	w.VarInt(itemID) // item id
	w.VarInt(0)      // components to add
	w.VarInt(0)      // components to remove
	return w.Bytes()
}

// ParseKeepAlive extracts the keep-alive ID.
func ParseKeepAlive(body []byte) (int64, error) {
	return mcwire.NewReader(body).Int64BE()
}

// ParsePing extracts the play/config ping ID (int).
func ParsePing(body []byte) (int32, error) {
	return mcwire.NewReader(body).Int32BE()
}

// BlockUpdate is the clientbound block_update packet: a position and the block
// state now there.
type BlockUpdate struct {
	X, Y, Z int32
	StateID int32
}

// AirStateID is the global block-state id of minecraft:air, which is always 0.
// A block_update carrying it at a position we dug is the server confirming the
// block is gone.
const AirStateID int32 = 0

// ParseBlockUpdate decodes a clientbound block_update body.
func ParseBlockUpdate(body []byte) (BlockUpdate, error) {
	var b BlockUpdate
	r := mcwire.NewReader(body)
	packed, err := r.Int64BE()
	if err != nil {
		return b, err
	}
	b.X, b.Y, b.Z = unpackBlockPos(packed)
	b.StateID, err = r.VarInt()
	return b, err
}

// BlockPlaceRequest is the serverbound use_item_on as the server reads it: the
// block that was clicked and which face of it. The new block goes one step
// along that face — the position is not the placed block.
type BlockPlaceRequest struct {
	Hand    int32
	X, Y, Z int32
	Face    int32
}

// ParseBlockPlace decodes a use_item_on body. Used by tests to check the client
// asks for what it means to ask for; the real server does the same arithmetic.
func ParseBlockPlace(body []byte) (BlockPlaceRequest, error) {
	var p BlockPlaceRequest
	r := mcwire.NewReader(body)
	var err error
	if p.Hand, err = r.VarInt(); err != nil {
		return p, err
	}
	packed, err := r.Int64BE()
	if err != nil {
		return p, err
	}
	p.X, p.Y, p.Z = unpackBlockPos(packed)
	p.Face, err = r.VarInt()
	return p, err
}

// unpackBlockPos reverses blockPos. Each field is sign-extended from its own
// width: x and z are 26-bit, y is 12-bit, and treating them as unsigned would
// put every negative coordinate tens of millions of blocks away.
func unpackBlockPos(v int64) (x, y, z int32) {
	x = int32(signExtend(v>>38, 26))
	z = int32(signExtend((v>>12)&0x3FFFFFF, 26))
	y = int32(signExtend(v&0xFFF, 12))
	return
}

func signExtend(v int64, bits uint) int64 {
	shift := 64 - bits
	return (v << shift) >> shift
}
