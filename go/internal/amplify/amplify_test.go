package amplify

import (
	"bytes"
	"testing"

	"mcbench/internal/rawevent"
	"mcbench/internal/tracefile"
)

func srcTrace() *tracefile.Trace {
	return &tracefile.Trace{
		SchemaVersion: tracefile.SchemaVersion, ProtocolVersion: 775,
		WorldProfileID: "p", TraceID: "src", DurationUs: 10_000_000,
		Events: []tracefile.TraceEvent{
			{OffsetUs: 0, Kind: rawevent.KindMarker, Data: rawevent.EncodeCmd("start")},
			{OffsetUs: 1_000_000, Kind: rawevent.KindDig,
				Data: rawevent.DigPayload{Action: 2, X: 100, Y: 64, Z: -50, Face: 1}.Encode()},
			{OffsetUs: 2_000_000, Kind: rawevent.KindCmd, Data: rawevent.EncodeCmd("/ah sell 100")},
			{OffsetUs: 3_000_000, Kind: rawevent.KindCmd, Data: rawevent.EncodeCmd("/eco give {SELF} 100000")},
			{OffsetUs: 10_000_000, Kind: rawevent.KindMarker, Data: rawevent.EncodeCmd("end")},
		},
	}
}

func TestAmplifyPreservesStructure(t *testing.T) {
	src := srcTrace()
	opt := Options{StartJitterUs: 5_000_000, EventJitterUs: 100_000, SpaceSpread: 300, VaryPercent: 25}
	got := Trace(src, "amp-1", opt, NewRNG(42))

	if len(got.Events) != len(src.Events) {
		t.Fatalf("event count changed: %d vs %d", len(got.Events), len(src.Events))
	}
	if got.ProtocolVersion != src.ProtocolVersion || got.WorldProfileID != src.WorldProfileID {
		t.Errorf("header not preserved: %+v", got)
	}
	if got.TraceID != "amp-1" {
		t.Errorf("trace id = %q", got.TraceID)
	}
	// Offsets must be non-decreasing so the replay loop consumes them in order.
	for i := 1; i < len(got.Events); i++ {
		if got.Events[i].OffsetUs < got.Events[i-1].OffsetUs {
			t.Fatalf("offsets not sorted at %d: %v", i, got.Events)
		}
	}
	if got.DurationUs != got.Events[len(got.Events)-1].OffsetUs {
		t.Errorf("duration %d != last offset %d", got.DurationUs, got.Events[len(got.Events)-1].OffsetUs)
	}
}

func TestAmplifyOffsetsCoordinates(t *testing.T) {
	src := srcTrace()
	opt := Options{SpaceSpread: 300}
	got := Trace(src, "amp", opt, NewRNG(7))

	var found bool
	for _, e := range got.Events {
		if e.Kind != rawevent.KindDig {
			continue
		}
		found = true
		d, err := rawevent.DecodeDig(e.Data)
		if err != nil {
			t.Fatal(err)
		}
		if d.Y != 64 || d.Action != 2 || d.Face != 1 {
			t.Errorf("non-positional dig fields changed: %+v", d)
		}
		if d.X < 100-300 || d.X > 100+300 || d.Z < -50-300 || d.Z > -50+300 {
			t.Errorf("dig offset outside spread: %+v", d)
		}
	}
	if !found {
		t.Fatal("dig event missing after amplify")
	}
}

func TestAmplifyDesyncsClones(t *testing.T) {
	src := srcTrace()
	opt := Options{StartJitterUs: 30_000_000}
	rng := NewRNG(99)
	a := Trace(src, "a", opt, rng)
	b := Trace(src, "b", opt, rng)
	if a.Events[0].OffsetUs == b.Events[0].OffsetUs {
		t.Errorf("clones did not desync: both start at %d", a.Events[0].OffsetUs)
	}
}

func TestAmplifyDeterministic(t *testing.T) {
	src := srcTrace()
	opt := Options{StartJitterUs: 1_000_000, SpaceSpread: 100, VaryPercent: 20}
	a := Trace(src, "x", opt, NewRNG(5))
	b := Trace(src, "x", opt, NewRNG(5))
	for i := range a.Events {
		if a.Events[i].OffsetUs != b.Events[i].OffsetUs || !bytes.Equal(a.Events[i].Data, b.Events[i].Data) {
			t.Fatalf("same seed produced different output at event %d", i)
		}
	}
}

func TestVaryNumbersPreservesTokens(t *testing.T) {
	rng := NewRNG(3)
	// {SELF} and non-numeric words must survive; the number may change.
	got := VaryNumbers("/eco give {SELF} 100000", 30, rng)
	if !bytes.Contains([]byte(got), []byte("{SELF}")) {
		t.Errorf("{SELF} token lost: %q", got)
	}
	if !bytes.HasPrefix([]byte(got), []byte("/eco give {SELF} ")) {
		t.Errorf("command shape changed: %q", got)
	}
	// Zero percent leaves the command untouched.
	if s := VaryNumbers("/ah sell 100", 0, rng); s != "/ah sell 100" {
		t.Errorf("pct=0 changed command: %q", s)
	}
	// Values stay >= 1 even with extreme variation.
	for i := 0; i < 50; i++ {
		s := VaryNumbers("/ah sell 2", 100, rng)
		if s == "/ah sell 0" || s == "/ah sell -1" {
			t.Fatalf("produced invalid price: %q", s)
		}
	}
}
