// Package rawevent defines the RawEvent model produced by the Paper capture
// plugin, plus its binary encoding. The encoding must stay byte-compatible
// with the Java side (capture-plugin). See docs/FORMAT.md section 2.
package rawevent

import (
	"fmt"

	"mcbench/internal/mcwire"
)

// Event kinds, shared between Java and Go.
const (
	KindMove           int32 = 0
	KindSprintToggle   int32 = 1
	KindSneakToggle    int32 = 2
	KindDig            int32 = 3
	KindPlaceBlock     int32 = 4
	KindUseItem        int32 = 5
	KindInteractEntity int32 = 6
	KindAttackEntity   int32 = 7
	KindInvOpen        int32 = 8
	KindInvClick       int32 = 9
	KindInvClose       int32 = 10
	KindCmd            int32 = 11
	KindMobSpawn       int32 = 12
	KindMobDespawn     int32 = 13
	KindMarker         int32 = 14
	// KindCreativeSet reproduces a creative-mode inventory set (server writes the
	// item straight into the slot). Payload: slot, item_id, count (3 VarInts).
	KindCreativeSet int32 = 15
)

var kindNames = map[int32]string{
	KindMove: "move", KindSprintToggle: "sprint_toggle", KindSneakToggle: "sneak_toggle",
	KindDig: "dig", KindPlaceBlock: "place_block", KindUseItem: "use_item",
	KindInteractEntity: "interact_entity", KindAttackEntity: "attack_entity",
	KindInvOpen: "inv_open", KindInvClick: "inv_click", KindInvClose: "inv_close",
	KindCmd: "cmd", KindMobSpawn: "mob_spawn", KindMobDespawn: "mob_despawn",
	KindMarker: "marker",
}

func KindName(k int32) string {
	if n, ok := kindNames[k]; ok {
		return n
	}
	return fmt.Sprintf("kind_%d", k)
}

// RawEvent is one captured event.
type RawEvent struct {
	TMicro       int64    // microseconds since plugin start
	PlayerID     [32]byte // SHA-256(player UUID + salt)
	SessionSeq   int32    // per-player login sequence
	DimensionID  int32
	CoarseChunkX int32
	CoarseChunkZ int32
	RegionID     string
	Kind         int32
	Payload      []byte // kind-specific encoding, see docs/FORMAT.md 2.2
}

// Encode appends the event (without the outer length prefix) to w.
func (e *RawEvent) Encode(w *mcwire.Writer) {
	w.Int64BE(e.TMicro)
	w.Raw(e.PlayerID[:])
	w.VarInt(e.SessionSeq)
	w.VarInt(e.DimensionID)
	w.VarInt(e.CoarseChunkX)
	w.VarInt(e.CoarseChunkZ)
	w.String(e.RegionID)
	w.VarInt(e.Kind)
	w.VarInt(int32(len(e.Payload)))
	w.Raw(e.Payload)
}

// Decode reads one event from r (which must contain exactly one event body).
func Decode(r *mcwire.Reader) (RawEvent, error) {
	var e RawEvent
	var err error
	if e.TMicro, err = r.Int64BE(); err != nil {
		return e, fmt.Errorf("t_micro: %w", err)
	}
	id, err := r.Bytes(32)
	if err != nil {
		return e, fmt.Errorf("player_id: %w", err)
	}
	copy(e.PlayerID[:], id)
	if e.SessionSeq, err = r.VarInt(); err != nil {
		return e, fmt.Errorf("session_seq: %w", err)
	}
	if e.DimensionID, err = r.VarInt(); err != nil {
		return e, fmt.Errorf("dimension_id: %w", err)
	}
	if e.CoarseChunkX, err = r.VarInt(); err != nil {
		return e, fmt.Errorf("coarse_chunk_x: %w", err)
	}
	if e.CoarseChunkZ, err = r.VarInt(); err != nil {
		return e, fmt.Errorf("coarse_chunk_z: %w", err)
	}
	if e.RegionID, err = r.String(); err != nil {
		return e, fmt.Errorf("region_id: %w", err)
	}
	if e.Kind, err = r.VarInt(); err != nil {
		return e, fmt.Errorf("kind: %w", err)
	}
	plen, err := r.VarInt()
	if err != nil {
		return e, fmt.Errorf("payload len: %w", err)
	}
	if plen < 0 {
		return e, fmt.Errorf("negative payload length %d", plen)
	}
	p, err := r.Bytes(int(plen))
	if err != nil {
		return e, fmt.Errorf("payload: %w", err)
	}
	e.Payload = append([]byte(nil), p...)
	return e, nil
}

// ---- payload helpers (used by the replay client and fixtures) ----

// MovePayload is the decoded EVENT_MOVE payload.
type MovePayload struct {
	DX, DY, DZ float32
	Yaw, Pitch float32
	OnGround   bool
}

func (m MovePayload) Encode() []byte {
	w := mcwire.NewWriter()
	w.Float32LE(m.DX)
	w.Float32LE(m.DY)
	w.Float32LE(m.DZ)
	w.Float32LE(m.Yaw)
	w.Float32LE(m.Pitch)
	w.Bool(m.OnGround)
	return w.Bytes()
}

func DecodeMove(p []byte) (MovePayload, error) {
	r := mcwire.NewReader(p)
	var m MovePayload
	var err error
	if m.DX, err = r.Float32LE(); err != nil {
		return m, err
	}
	if m.DY, err = r.Float32LE(); err != nil {
		return m, err
	}
	if m.DZ, err = r.Float32LE(); err != nil {
		return m, err
	}
	if m.Yaw, err = r.Float32LE(); err != nil {
		return m, err
	}
	if m.Pitch, err = r.Float32LE(); err != nil {
		return m, err
	}
	m.OnGround, err = r.Bool()
	return m, err
}

// DigPayload is the decoded EVENT_DIG payload.
type DigPayload struct {
	Action  int32 // 0=start 1=cancel 2=finish
	X, Y, Z int32
	Face    int32
}

func (d DigPayload) Encode() []byte {
	w := mcwire.NewWriter()
	w.VarInt(d.Action)
	w.VarInt(d.X)
	w.VarInt(d.Y)
	w.VarInt(d.Z)
	w.VarInt(d.Face)
	return w.Bytes()
}

func DecodeDig(p []byte) (DigPayload, error) {
	r := mcwire.NewReader(p)
	var d DigPayload
	var err error
	if d.Action, err = r.VarInt(); err != nil {
		return d, err
	}
	if d.X, err = r.VarInt(); err != nil {
		return d, err
	}
	if d.Y, err = r.VarInt(); err != nil {
		return d, err
	}
	if d.Z, err = r.VarInt(); err != nil {
		return d, err
	}
	d.Face, err = r.VarInt()
	return d, err
}

// PlacePayload is the decoded EVENT_PLACE_BLOCK payload.
type PlacePayload struct {
	X, Y, Z int32
	Face    int32
	Hand    int32
}

func (d PlacePayload) Encode() []byte {
	w := mcwire.NewWriter()
	w.VarInt(d.X)
	w.VarInt(d.Y)
	w.VarInt(d.Z)
	w.VarInt(d.Face)
	w.VarInt(d.Hand)
	return w.Bytes()
}

func DecodePlace(p []byte) (PlacePayload, error) {
	r := mcwire.NewReader(p)
	var d PlacePayload
	var err error
	if d.X, err = r.VarInt(); err != nil {
		return d, err
	}
	if d.Y, err = r.VarInt(); err != nil {
		return d, err
	}
	if d.Z, err = r.VarInt(); err != nil {
		return d, err
	}
	if d.Face, err = r.VarInt(); err != nil {
		return d, err
	}
	d.Hand, err = r.VarInt()
	return d, err
}

// UseItemPayload is the decoded EVENT_USE_ITEM payload.
type UseItemPayload struct {
	Hand   int32
	ItemID int32
}

func (d UseItemPayload) Encode() []byte {
	w := mcwire.NewWriter()
	w.VarInt(d.Hand)
	w.VarInt(d.ItemID)
	return w.Bytes()
}

func DecodeUseItem(p []byte) (UseItemPayload, error) {
	r := mcwire.NewReader(p)
	var d UseItemPayload
	var err error
	if d.Hand, err = r.VarInt(); err != nil {
		return d, err
	}
	d.ItemID, err = r.VarInt()
	return d, err
}

// TogglePayload decodes sprint/sneak toggles.
func DecodeToggle(p []byte) (bool, error) {
	r := mcwire.NewReader(p)
	return r.Bool()
}

func EncodeToggle(on bool) []byte {
	if on {
		return []byte{1}
	}
	return []byte{0}
}

// CmdPayload decodes EVENT_CMD to the command string (including leading '/').
func DecodeCmd(p []byte) (string, error) {
	r := mcwire.NewReader(p)
	return r.String()
}

func EncodeCmd(cmd string) []byte {
	w := mcwire.NewWriter()
	w.String(cmd)
	return w.Bytes()
}

// DecodeMarker decodes EVENT_MARKER to the marker string.
func DecodeMarker(p []byte) (string, error) {
	r := mcwire.NewReader(p)
	return r.String()
}

// CreativeSetPayload is the decoded KindCreativeSet payload.
type CreativeSetPayload struct {
	Slot   int32
	ItemID int32
	Count  int32
}

func (d CreativeSetPayload) Encode() []byte {
	w := mcwire.NewWriter()
	w.VarInt(d.Slot)
	w.VarInt(d.ItemID)
	w.VarInt(d.Count)
	return w.Bytes()
}

func DecodeCreativeSet(p []byte) (CreativeSetPayload, error) {
	r := mcwire.NewReader(p)
	var d CreativeSetPayload
	var err error
	if d.Slot, err = r.VarInt(); err != nil {
		return d, err
	}
	if d.ItemID, err = r.VarInt(); err != nil {
		return d, err
	}
	d.Count, err = r.VarInt()
	return d, err
}

// InvClickPayload is the decoded EVENT_INV_CLICK payload. The captured
// window_id is intentionally ignored by replay (a live id is used instead).
type InvClickPayload struct {
	WindowID  int32
	Slot      int32
	Button    int32
	ClickType int32
}

func (d InvClickPayload) Encode() []byte {
	w := mcwire.NewWriter()
	w.VarInt(d.WindowID)
	w.VarInt(d.Slot)
	w.VarInt(d.Button)
	w.VarInt(d.ClickType)
	return w.Bytes()
}

func DecodeInvClick(p []byte) (InvClickPayload, error) {
	r := mcwire.NewReader(p)
	var d InvClickPayload
	var err error
	if d.WindowID, err = r.VarInt(); err != nil {
		return d, err
	}
	if d.Slot, err = r.VarInt(); err != nil {
		return d, err
	}
	if d.Button, err = r.VarInt(); err != nil {
		return d, err
	}
	d.ClickType, err = r.VarInt()
	return d, err
}

// InvOpenPayload is the decoded EVENT_INV_OPEN payload. HasPos indicates the
// capture recorded the container's block position (block containers); false for
// the player's own inventory or containers without a world location.
type InvOpenPayload struct {
	ContainerType int32
	HasPos        bool
	X, Y, Z       int32
}

func (d InvOpenPayload) Encode() []byte {
	w := mcwire.NewWriter()
	w.VarInt(d.ContainerType)
	w.Bool(d.HasPos)
	if d.HasPos {
		w.VarInt(d.X)
		w.VarInt(d.Y)
		w.VarInt(d.Z)
	}
	return w.Bytes()
}

func DecodeInvOpen(p []byte) (InvOpenPayload, error) {
	r := mcwire.NewReader(p)
	var d InvOpenPayload
	var err error
	if d.ContainerType, err = r.VarInt(); err != nil {
		return d, err
	}
	if d.HasPos, err = r.Bool(); err != nil {
		return d, err
	}
	if d.HasPos {
		if d.X, err = r.VarInt(); err != nil {
			return d, err
		}
		if d.Y, err = r.VarInt(); err != nil {
			return d, err
		}
		if d.Z, err = r.VarInt(); err != nil {
			return d, err
		}
	}
	return d, nil
}
