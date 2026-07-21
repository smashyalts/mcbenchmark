package replay

import (
	"math"
	"sync"

	"mcbench/internal/mcproto"
)

// entityTracker holds the live entities the server has told this client about.
//
// A capture cannot record which entity a player hit in any way replay can use.
// Entity ids are assigned per server run, so the captured one means nothing on
// the benchmark server, and there is no way to look one up by name. That left
// attacks replayed as a bare arm swing: an animation, and nothing else. No
// damage, no aggro, no death, no drops, no XP — every downstream cost of combat
// missing from a benchmark whose whole job is to reproduce cost.
//
// The server does tell the client, though: add_entity carries an id, a type and
// a position for everything that comes into view. Keeping that means an attack
// can be aimed at a live entity of the same kind the capture attacked, which is
// as close to the original as anything short of a shared world can get.
type entityTracker struct {
	mu sync.Mutex
	// Bounded by view distance: the server sends remove_entities as things
	// leave, and the map is cleared on respawn/world change.
	byID map[int32]trackedEntity
}

type trackedEntity struct {
	typeID  int32
	x, y, z float64
}

func newEntityTracker() *entityTracker {
	return &entityTracker{byID: make(map[int32]trackedEntity)}
}

func (t *entityTracker) add(a mcproto.AddEntity) {
	t.mu.Lock()
	t.byID[a.EntityID] = trackedEntity{typeID: a.TypeID, x: a.X, y: a.Y, z: a.Z}
	t.mu.Unlock()
}

func (t *entityTracker) remove(ids []int32) {
	t.mu.Lock()
	for _, id := range ids {
		delete(t.byID, id)
	}
	t.mu.Unlock()
}

// attackRangeSq is the squared distance within which the server will accept an
// attack. Vanilla allows a little over 3 blocks for melee and rejects beyond 6;
// 36 is that hard ceiling, so anything this picks is at worst refused, never
// treated as reach-hacking.
const attackRangeSq = 36.0

// nearest returns the closest entity within attack range, preferring one whose
// type matches wantType. It reports whether the match was exact.
//
// The preference matters more than it looks: attacking a zombie and attacking a
// villager cost the server very different amounts of work downstream — pathing,
// aggro, drops, XP. Falling back to any nearby entity keeps the combat load
// present rather than absent, and the run reports the two cases separately so
// "we hit something, but not the right kind of something" is never mistaken for
// a faithful replay.
func (t *entityTracker) nearest(x, y, z float64, wantType int32) (id int32, exact, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	bestExact, bestAny := math.Inf(1), math.Inf(1)
	var idExact, idAny int32
	var haveExact, haveAny bool
	for eid, e := range t.byID {
		dx, dy, dz := e.x-x, e.y-y, e.z-z
		d := dx*dx + dy*dy + dz*dz
		if d > attackRangeSq {
			continue
		}
		if e.typeID == wantType && d < bestExact {
			bestExact, idExact, haveExact = d, eid, true
		}
		if d < bestAny {
			bestAny, idAny, haveAny = d, eid, true
		}
	}
	if haveExact {
		return idExact, true, true
	}
	if haveAny {
		return idAny, false, true
	}
	return 0, false, false
}
