// Package tracefile implements the compiled per-session trace format and the
// run manifest. A trace file is:
//
//	magic "MCT1" | LZ4 frame of:
//	  schema_version VarInt
//	  protocol_version VarInt
//	  world_profile String
//	  trace_id String
//	  duration_us VarLong
//	  [schema >= 2] origin: has_origin Bool
//	                        if set: x,y,z Float64BE, yaw,pitch Float32BE,
//	                                dimension VarInt, exact Bool
//	  [schema >= 3] inventory: has_inventory Bool
//	                        if set: selected_slot VarInt, item_count VarInt,
//	                                per item: slot VarInt, id String, count VarInt
//	  event_count VarInt
//	  events: [delta_offset_us VarLong][kind VarInt][data_len VarInt][data]
//
// Event payload ("data") encodings are identical to RawEvent payloads
// (docs/FORMAT.md 2.2); offsets are delta-encoded microseconds.
package tracefile

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/pierrec/lz4/v4"

	"mcbench/internal/mcwire"
	"mcbench/internal/rawevent"
)

var magic = []byte("MCT1")

// SchemaVersion 2 added the session origin, 3 the login inventory. Older files
// still read: they simply carry neither, and every consumer treats that as
// "unknown".
const SchemaVersion = 3

type TraceEvent struct {
	OffsetUs int64
	Kind     int32
	Data     []byte
}

// Origin is where the captured player stood when the session began.
//
// Replay bots cannot choose their own spawn — the server decides, and without
// this a bot lands at world spawn: out of interaction range of every block its
// trace digs or places, and (unless world spawn happens to be solid ground)
// kicked for "flying" after four seconds of hovering. bench-playerdata writes
// this position into each bench account's player data so the bot is already in
// the captured region the moment it logs in.
//
// Exact is false when the position was inferred rather than captured — from a
// block the player interacted with, or from the coarse chunk stamped on every
// event. Inferred origins are good enough to put a bot in the right region;
// only an exact one guarantees interaction range.
type Origin struct {
	X, Y, Z    float64
	Yaw, Pitch float32
	Dimension  int32
	Exact      bool
}

// Inventory is a captured login inventory. Slots are Bukkit indices (0-35 main,
// 36-39 armor boots-first, 40 offhand).
type Inventory struct {
	SelectedSlot int32
	Items        []rawevent.ItemStack
}

type Trace struct {
	SchemaVersion   uint32
	ProtocolVersion uint32
	WorldProfileID  string
	TraceID         string
	DurationUs      int64
	// Origin is nil when the trace predates schema 2 or the compiler could not
	// resolve a position.
	Origin *Origin
	// Inventory is what the captured player was carrying at login, written into
	// the bot's player data by bench-playerdata. Nil when unknown — in which case
	// the bot mines barehanded, which is a 20x error in block-break time against
	// the diamond pickaxe the capture may have used.
	Inventory *Inventory
	Events    []TraceEvent
}

// Write serializes and writes the trace to path.
func (t *Trace) Write(path string) error {
	w := mcwire.NewWriter()
	w.VarInt(int32(t.SchemaVersion))
	w.VarInt(int32(t.ProtocolVersion))
	w.String(t.WorldProfileID)
	w.String(t.TraceID)
	w.VarLong(t.DurationUs)
	// The origin block belongs to schema 2 onwards. Writing it under a schema-1
	// header would produce a file whose declared version does not match its
	// layout, and every reader would then mis-parse the event list.
	if t.SchemaVersion < 2 {
		if t.Origin != nil {
			return fmt.Errorf("trace %s: origin requires schema >= 2, have %d",
				t.TraceID, t.SchemaVersion)
		}
	} else if t.Origin != nil {
		w.Bool(true)
		w.Float64BE(t.Origin.X)
		w.Float64BE(t.Origin.Y)
		w.Float64BE(t.Origin.Z)
		w.Float32BE(t.Origin.Yaw)
		w.Float32BE(t.Origin.Pitch)
		w.VarInt(t.Origin.Dimension)
		w.Bool(t.Origin.Exact)
	} else {
		w.Bool(false)
	}
	if t.SchemaVersion >= 3 {
		if t.Inventory != nil {
			w.Bool(true)
			w.VarInt(t.Inventory.SelectedSlot)
			w.VarInt(int32(len(t.Inventory.Items)))
			for _, it := range t.Inventory.Items {
				w.VarInt(it.Slot)
				w.String(it.ID)
				w.VarInt(it.Count)
			}
		} else {
			w.Bool(false)
		}
	} else if t.Inventory != nil {
		return fmt.Errorf("trace %s: inventory requires schema >= 3, have %d",
			t.TraceID, t.SchemaVersion)
	}
	w.VarInt(int32(len(t.Events)))
	prev := int64(0)
	for _, e := range t.Events {
		w.VarLong(e.OffsetUs - prev)
		prev = e.OffsetUs
		w.VarInt(e.Kind)
		w.VarInt(int32(len(e.Data)))
		w.Raw(e.Data)
	}
	var out bytes.Buffer
	out.Write(magic)
	zw := lz4.NewWriter(&out)
	if _, err := zw.Write(w.Bytes()); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return os.WriteFile(path, out.Bytes(), 0o644)
}

// Read loads a trace file from path.
func Read(path string) (*Trace, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < 4 || !bytes.Equal(data[:4], magic) {
		return nil, fmt.Errorf("%s: not a trace file (bad magic)", path)
	}
	raw, err := io.ReadAll(lz4.NewReader(bytes.NewReader(data[4:])))
	if err != nil {
		return nil, fmt.Errorf("%s: decompress: %w", path, err)
	}
	r := mcwire.NewReader(raw)
	t := &Trace{}
	sv, err := r.VarInt()
	if err != nil {
		return nil, fmt.Errorf("%s: schema: %w", path, err)
	}
	t.SchemaVersion = uint32(sv)
	// Schema 1 traces stay readable: they predate the origin field and are
	// otherwise byte-identical. Refusing them would strand every trace compiled
	// before this change for no reason.
	if t.SchemaVersion < 1 || t.SchemaVersion > SchemaVersion {
		return nil, fmt.Errorf("%s: unsupported trace schema %d", path, t.SchemaVersion)
	}
	pv, err := r.VarInt()
	if err != nil {
		return nil, err
	}
	t.ProtocolVersion = uint32(pv)
	if t.WorldProfileID, err = r.String(); err != nil {
		return nil, err
	}
	if t.TraceID, err = r.String(); err != nil {
		return nil, err
	}
	if t.DurationUs, err = r.VarLong(); err != nil {
		return nil, err
	}
	if t.SchemaVersion >= 2 {
		has, err := r.Bool()
		if err != nil {
			return nil, fmt.Errorf("%s: origin flag: %w", path, err)
		}
		if has {
			o := &Origin{}
			if o.X, err = r.Float64BE(); err != nil {
				return nil, err
			}
			if o.Y, err = r.Float64BE(); err != nil {
				return nil, err
			}
			if o.Z, err = r.Float64BE(); err != nil {
				return nil, err
			}
			if o.Yaw, err = r.Float32BE(); err != nil {
				return nil, err
			}
			if o.Pitch, err = r.Float32BE(); err != nil {
				return nil, err
			}
			if o.Dimension, err = r.VarInt(); err != nil {
				return nil, err
			}
			if o.Exact, err = r.Bool(); err != nil {
				return nil, err
			}
			t.Origin = o
		}
	}
	if t.SchemaVersion >= 3 {
		has, err := r.Bool()
		if err != nil {
			return nil, fmt.Errorf("%s: inventory flag: %w", path, err)
		}
		if has {
			inv := &Inventory{}
			if inv.SelectedSlot, err = r.VarInt(); err != nil {
				return nil, err
			}
			cnt, err := r.VarInt()
			if err != nil {
				return nil, err
			}
			if cnt < 0 {
				return nil, fmt.Errorf("%s: negative inventory size %d", path, cnt)
			}
			for i := int32(0); i < cnt; i++ {
				var it rawevent.ItemStack
				if it.Slot, err = r.VarInt(); err != nil {
					return nil, err
				}
				if it.ID, err = r.String(); err != nil {
					return nil, err
				}
				if it.Count, err = r.VarInt(); err != nil {
					return nil, err
				}
				inv.Items = append(inv.Items, it)
			}
			t.Inventory = inv
		}
	}
	n, err := r.VarInt()
	if err != nil {
		return nil, err
	}
	if n < 0 {
		return nil, fmt.Errorf("%s: negative event count", path)
	}
	t.Events = make([]TraceEvent, 0, n)
	off := int64(0)
	for i := int32(0); i < n; i++ {
		d, err := r.VarLong()
		if err != nil {
			return nil, fmt.Errorf("%s: event %d offset: %w", path, i, err)
		}
		off += d
		kind, err := r.VarInt()
		if err != nil {
			return nil, fmt.Errorf("%s: event %d kind: %w", path, i, err)
		}
		dlen, err := r.VarInt()
		if err != nil {
			return nil, fmt.Errorf("%s: event %d data len: %w", path, i, err)
		}
		db, err := r.Bytes(int(dlen))
		if err != nil {
			return nil, fmt.Errorf("%s: event %d data: %w", path, i, err)
		}
		t.Events = append(t.Events, TraceEvent{OffsetUs: off, Kind: kind, Data: append([]byte(nil), db...)})
	}
	return t, nil
}

// Manifest describes a compiled trace set.
type Manifest struct {
	SchemaVersion   int             `json:"schema_version"`
	ProtocolVersion int             `json:"protocol_version"`
	WorldProfile    string          `json:"world_profile"`
	RunID           string          `json:"run_id"`
	Traces          []ManifestEntry `json:"traces"`
}

type ManifestEntry struct {
	File      string   `json:"file"`
	DurationS int64    `json:"duration_s"`
	Events    int      `json:"events"`
	Tags      []string `json:"tags"`
}

func (m *Manifest) Save(dir string) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "manifest.json"), append(b, '\n'), 0o644)
}

func LoadManifest(path string) (*Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(m.Traces) == 0 {
		return nil, fmt.Errorf("%s: manifest lists no traces", path)
	}
	return &m, nil
}
