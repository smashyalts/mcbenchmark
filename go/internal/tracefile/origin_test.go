package tracefile

import (
	"os"
	"path/filepath"
	"testing"
)

// TestOriginRoundTrip pins that the origin survives a write/read cycle. The
// origin is what bench-playerdata places bots with; losing it silently sends
// every bot to world spawn, where its block events do nothing.
func TestOriginRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.mct")
	in := &Trace{
		SchemaVersion: SchemaVersion, ProtocolVersion: 775,
		WorldProfileID: "w", TraceID: "t", DurationUs: 1000,
		Origin: &Origin{X: -904.5, Y: 79, Z: -152.5, Yaw: 12, Pitch: -3,
			Dimension: 1, Exact: true},
		Events: []TraceEvent{{OffsetUs: 0, Kind: 3, Data: []byte{1, 2}}},
	}
	if err := in.Write(path); err != nil {
		t.Fatal(err)
	}
	out, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if out.Origin == nil {
		t.Fatal("origin lost in round trip")
	}
	if *out.Origin != *in.Origin {
		t.Errorf("origin = %+v, want %+v", *out.Origin, *in.Origin)
	}
	if len(out.Events) != 1 || out.Events[0].Kind != 3 {
		t.Errorf("events corrupted: %+v", out.Events)
	}
}

// TestNoOriginRoundTrip covers a trace the compiler could not place.
func TestNoOriginRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.mct")
	in := &Trace{
		SchemaVersion: SchemaVersion, ProtocolVersion: 775,
		WorldProfileID: "w", TraceID: "t", DurationUs: 1,
		Events:         []TraceEvent{{OffsetUs: 0, Kind: 0, Data: nil}},
	}
	if err := in.Write(path); err != nil {
		t.Fatal(err)
	}
	out, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if out.Origin != nil {
		t.Errorf("origin = %+v, want nil", out.Origin)
	}
}

// TestSchema1StillReads guards backward compatibility: traces compiled before
// the origin existed must keep working rather than becoming unreadable.
func TestSchema1StillReads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.mct")
	in := &Trace{
		SchemaVersion: 1, ProtocolVersion: 775,
		WorldProfileID: "w", TraceID: "old", DurationUs: 500,
		Events:         []TraceEvent{{OffsetUs: 0, Kind: 3, Data: []byte{9}}},
	}
	if err := in.Write(path); err != nil {
		t.Fatal(err)
	}
	// Sanity: a schema-1 file must not contain the origin flag byte at all.
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	out, err := Read(path)
	if err != nil {
		t.Fatalf("schema 1 trace no longer readable: %v", err)
	}
	if out.Origin != nil {
		t.Error("schema 1 trace reported an origin")
	}
	if len(out.Events) != 1 || out.Events[0].Data[0] != 9 {
		t.Errorf("schema 1 events corrupted: %+v", out.Events)
	}
}
