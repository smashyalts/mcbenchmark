package rawlog

import (
	"errors"
	"os"
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

// A capture file cut off mid-frame is the normal result of killing a server, and
// it must cost only the incomplete frame — not the events already written to it,
// and above all not the other files in the directory.
func TestTruncatedFileKeepsEarlierFramesAndOtherFiles(t *testing.T) {
	dir := t.TempDir()
	var pid [32]byte
	ev := func(t int64) rawevent.RawEvent {
		return rawevent.RawEvent{TMicro: t, PlayerID: pid, Kind: rawevent.KindCmd,
			Payload: rawevent.EncodeCmd("/spawn")}
	}

	// Two frames in one file, then lop off the tail of the second.
	victim := filepath.Join(dir, "raw-20260101-000000-s0.bin")
	if err := WriteFile(victim, "test-server", 1, 2, []rawevent.RawEvent{ev(1)}); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(victim, "test-server", 3, 4, []rawevent.RawEvent{ev(2)}); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	// One whole frame, then half of the next.
	cut := append(append([]byte{}, first...), second[:len(second)/2]...)
	if err := os.WriteFile(victim, cut, 0o644); err != nil {
		t.Fatal(err)
	}

	// A second, intact file that must survive the first one's damage.
	intact := filepath.Join(dir, "raw-20260101-000001-s0.bin")
	if err := WriteFile(intact, "test-server", 5, 6, []rawevent.RawEvent{ev(3)}); err != nil {
		t.Fatal(err)
	}

	var seen []int64
	if err := StreamDir(dir, func(e rawevent.RawEvent) error {
		seen = append(seen, e.TMicro)
		return nil
	}); err != nil {
		t.Fatalf("StreamDir should tolerate a truncated tail, got %v", err)
	}
	if len(seen) != 2 || seen[0] != 1 || seen[1] != 3 {
		t.Fatalf("want the complete frame and the intact file (1, 3), got %v", seen)
	}

	// Streaming the damaged file alone still reports what happened.
	err = Stream(victim, func(rawevent.RawEvent) error { return nil })
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("want ErrTruncated from the damaged file, got %v", err)
	}
}

// A corrupt length prefix must not be believed: it is read before any
// validation and would otherwise size an allocation.
func TestImplausibleFrameLengthIsRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "raw-20260101-000000-s0.bin")
	if err := os.WriteFile(path, []byte{0xFF, 0xFF, 0xFF, 0xFF, 0x00}, 0o644); err != nil {
		t.Fatal(err)
	}
	err := Stream(path, func(rawevent.RawEvent) error { return nil })
	if err == nil || errors.Is(err, ErrTruncated) {
		t.Fatalf("want a hard error for a 4 GiB frame length, got %v", err)
	}
}
