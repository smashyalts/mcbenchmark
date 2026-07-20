package tracefile

import (
	"path/filepath"
	"testing"
)

func TestTraceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trace-000001.bin")
	tr := &Trace{
		SchemaVersion:   SchemaVersion,
		ProtocolVersion: 775,
		WorldProfileID:  "bench-arena-v1",
		TraceID:         "run-000001",
		DurationUs:      5_000_000,
		Events: []TraceEvent{
			{OffsetUs: 0, Kind: 0, Data: []byte{1, 2, 3}},
			{OffsetUs: 1_000_000, Kind: 11, Data: []byte("hi")},
			{OffsetUs: 5_000_000, Kind: 3, Data: nil},
		},
	}
	if err := tr.Write(path); err != nil {
		t.Fatal(err)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.ProtocolVersion != 775 || got.WorldProfileID != "bench-arena-v1" || got.TraceID != "run-000001" {
		t.Errorf("header mismatch: %+v", got)
	}
	if len(got.Events) != 3 {
		t.Fatalf("got %d events", len(got.Events))
	}
	if got.Events[1].OffsetUs != 1_000_000 || got.Events[2].OffsetUs != 5_000_000 {
		t.Errorf("offset delta decode wrong: %+v", got.Events)
	}
	if string(got.Events[1].Data) != "hi" {
		t.Errorf("data mismatch: %q", got.Events[1].Data)
	}
}

func TestManifestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m := &Manifest{
		SchemaVersion:   1,
		ProtocolVersion: 775,
		WorldProfile:    "p",
		RunID:           "r",
		Traces:          []ManifestEntry{{File: "trace-000001.bin", DurationS: 30, Events: 5, Tags: []string{"combat"}}},
	}
	if err := m.Save(dir); err != nil {
		t.Fatal(err)
	}
	got, err := LoadManifest(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Traces) != 1 || got.Traces[0].File != "trace-000001.bin" {
		t.Errorf("manifest mismatch: %+v", got)
	}
}
