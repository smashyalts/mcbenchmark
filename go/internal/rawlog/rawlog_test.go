package rawlog

import (
	"path/filepath"
	"testing"

	"mcbench/internal/rawevent"
)

func TestCaptureLogRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "raw-20260101-0000.bin")

	var pid [32]byte
	pid[0] = 0xAB
	events := []rawevent.RawEvent{
		{TMicro: 1000, PlayerID: pid, SessionSeq: 0, DimensionID: 0, CoarseChunkX: 2, CoarseChunkZ: -3,
			RegionID: "arena", Kind: rawevent.KindMove,
			Payload: rawevent.MovePayload{DX: 0.1, DY: 0, DZ: -0.2, Yaw: 90, Pitch: 10, OnGround: true}.Encode()},
		{TMicro: 2000, PlayerID: pid, SessionSeq: 0, Kind: rawevent.KindCmd,
			Payload: rawevent.EncodeCmd("/spawn")},
		{TMicro: 3000, PlayerID: pid, SessionSeq: 0, Kind: rawevent.KindDig,
			Payload: rawevent.DigPayload{Action: 2, X: 10, Y: 64, Z: -5, Face: 1}.Encode()},
	}
	if err := WriteFile(path, "test-server", 1, 3, events); err != nil {
		t.Fatal(err)
	}
	got, err := ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(events) {
		t.Fatalf("got %d events, want %d", len(got), len(events))
	}
	for i := range events {
		if got[i].TMicro != events[i].TMicro || got[i].Kind != events[i].Kind ||
			got[i].RegionID != events[i].RegionID || got[i].CoarseChunkZ != events[i].CoarseChunkZ {
			t.Errorf("event %d mismatch: %+v vs %+v", i, got[i], events[i])
		}
	}
	m, err := rawevent.DecodeMove(got[0].Payload)
	if err != nil {
		t.Fatal(err)
	}
	if m.Yaw != 90 || !m.OnGround || m.DZ != -0.2 {
		t.Errorf("move payload mismatch: %+v", m)
	}
	cmd, err := rawevent.DecodeCmd(got[1].Payload)
	if err != nil || cmd != "/spawn" {
		t.Errorf("cmd payload: %q %v", cmd, err)
	}
}
