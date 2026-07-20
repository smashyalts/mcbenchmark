// Package tracefile implements the compiled per-session trace format and the
// run manifest. A trace file is:
//
//	magic "MCT1" | LZ4 frame of:
//	  schema_version VarInt
//	  protocol_version VarInt
//	  world_profile String
//	  trace_id String
//	  duration_us VarLong
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
)

var magic = []byte("MCT1")

const SchemaVersion = 1

type TraceEvent struct {
	OffsetUs int64
	Kind     int32
	Data     []byte
}

type Trace struct {
	SchemaVersion   uint32
	ProtocolVersion uint32
	WorldProfileID  string
	TraceID         string
	DurationUs      int64
	Events          []TraceEvent
}

// Write serializes and writes the trace to path.
func (t *Trace) Write(path string) error {
	w := mcwire.NewWriter()
	w.VarInt(int32(t.SchemaVersion))
	w.VarInt(int32(t.ProtocolVersion))
	w.String(t.WorldProfileID)
	w.String(t.TraceID)
	w.VarLong(t.DurationUs)
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
	if t.SchemaVersion != SchemaVersion {
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
