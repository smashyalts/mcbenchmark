package replay

import (
	"testing"

	"mcbench/internal/mcproto"
	"mcbench/internal/rawevent"
	"mcbench/internal/tracefile"
)

// traceEvent wraps a payload for dispatch. The session has no codec in these
// tests, so sends fail harmlessly and only the bookkeeping is under test.
func traceEvent(kind int32, data []byte) tracefile.TraceEvent {
	return tracefile.TraceEvent{Kind: kind, Data: data}
}

func TestExpandCommand(t *testing.T) {
	cases := []struct {
		cmd, user, want string
	}{
		{"/eco give {SELF} 100000", "DEMO_00007", "eco give DEMO_00007 100000"},
		{"/ah sell 100", "DEMO_00000", "ah sell 100"},
		{"/ah", "X", "ah"},
		{"{SELF} bare", "P", "P bare"},               // no leading slash
		{"/msg {SELF} hi {SELF}", "A", "msg A hi A"}, // multiple tokens
		{"/", "P", ""},                               // slash only -> empty
		{"", "P", ""},
	}
	for _, c := range cases {
		got := expandCommand(c.cmd, c.user)
		if got != c.want {
			t.Errorf("expandCommand(%q,%q) = %q, want %q", c.cmd, c.user, got, c.want)
		}
	}
}

// A trace carrying its own dig START must not also get a synthetic one: that
// would send two starts for one break, and the point of packet-level capture is
// that the break spans the ticks it really took.
func TestRealDigStartSuppressesTheSyntheticOne(t *testing.T) {
	s := &Session{agg: &Aggregate{}}
	s.digStarted = map[[3]int32]bool{}

	// Real start seen, then finish: nothing synthesised.
	s.dispatch(traceEvent(rawevent.KindDig, rawevent.DigPayload{
		Action: mcproto.DigStart, X: 1, Y: 2, Z: 3, Face: 1}.Encode()))
	s.dispatch(traceEvent(rawevent.KindDig, rawevent.DigPayload{
		Action: mcproto.DigFinish, X: 1, Y: 2, Z: 3, Face: 1}.Encode()))
	if n := s.agg.DigStartsSynthesised.Load(); n != 0 {
		t.Errorf("synthesised %d starts for a trace that supplied its own", n)
	}

	// A bare finish (an old trace) still gets one, or nothing would break.
	s.dispatch(traceEvent(rawevent.KindDig, rawevent.DigPayload{
		Action: mcproto.DigFinish, X: 9, Y: 9, Z: 9, Face: 1}.Encode()))
	if n := s.agg.DigStartsSynthesised.Load(); n != 1 {
		t.Errorf("bare finish should be given a start, synthesised %d", n)
	}
}

// An attack with nothing in range must be counted apart from one that landed:
// a swing at air reproduces none of combat's cost.
func TestAttackWithNoTargetIsCountedNotClaimed(t *testing.T) {
	s := &Session{agg: &Aggregate{}, entities: newEntityTracker()}
	s.dispatch(traceEvent(rawevent.KindAttackEntity,
		rawevent.EntityRef{TypeKey: "minecraft:zombie"}.Encode()))
	if s.agg.AttacksNoTarget.Load() != 1 || s.agg.AttacksOnType.Load() != 0 {
		t.Errorf("empty tracker should count no_target: on=%d off=%d none=%d",
			s.agg.AttacksOnType.Load(), s.agg.AttacksOffType.Load(),
			s.agg.AttacksNoTarget.Load())
	}
}

// The tracker must prefer the captured species, and say when it could not.
func TestNearestPrefersTheCapturedSpecies(t *testing.T) {
	tr := newEntityTracker()
	zombie := mcproto.EntityTypeID["minecraft:zombie"]
	pig := mcproto.EntityTypeID["minecraft:pig"]
	// The pig is closer, but the capture attacked a zombie.
	tr.add(mcproto.AddEntity{EntityID: 1, TypeID: pig, X: 0.5, Y: 0, Z: 0})
	tr.add(mcproto.AddEntity{EntityID: 2, TypeID: zombie, X: 2, Y: 0, Z: 0})
	id, exact, ok := tr.nearest(0, 0, 0, zombie)
	if !ok || id != 2 || !exact {
		t.Errorf("want the zombie (id 2, exact), got id=%d exact=%v ok=%v", id, exact, ok)
	}
	// With no zombie present, the pig is better than nothing — but not exact.
	tr.remove([]int32{2})
	id, exact, ok = tr.nearest(0, 0, 0, zombie)
	if !ok || id != 1 || exact {
		t.Errorf("want the pig as an inexact fallback, got id=%d exact=%v ok=%v", id, exact, ok)
	}
	// Out of reach is no target at all.
	tr.remove([]int32{1})
	tr.add(mcproto.AddEntity{EntityID: 3, TypeID: zombie, X: 40, Y: 0, Z: 0})
	if _, _, ok = tr.nearest(0, 0, 0, zombie); ok {
		t.Error("an entity 40 blocks away must not be attacked")
	}
}
