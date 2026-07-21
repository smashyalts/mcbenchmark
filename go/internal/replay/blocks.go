package replay

import (
	"sync"

	"mcbench/internal/mcproto"
	"mcbench/internal/rawevent"
	"mcbench/internal/tracefile"
)

// blockLedger is what the replay client knows about the world at the positions
// its trace touches.
//
// It exists because "the server sent a block_update saying air" is not evidence
// a dig worked. The server sends exactly that packet in two opposite cases: when
// it broke the block, and when it is correcting a client that asked to break a
// block which was already air. Without knowing the state beforehand the two are
// identical, and the second is not a corner case — it is what a *misplaced* bot
// produces. Most coordinates in a world are air, so a bot that spawned at world
// spawn and dug at its trace's coordinates would have reported every dig as a
// success. That is precisely the failure the dig counter was added to catch, so
// the counter was blind in the one case it mattered.
//
// Only the positions a trace digs or builds at are tracked, which is what keeps
// this affordable: hundreds of positions per trace, against the millions of
// blocks in the chunks a bot has in view.
type blockLedger struct {
	// want is fixed after construction and read without locking.
	want map[[3]int32]bool
	// agg is the run-wide counter set, so an unreadable chunk is visible in the
	// report rather than only inside one session.
	agg *Aggregate

	mu    sync.Mutex
	state map[[3]int32]int32
	// unparsed counts chunks whose format this client could not read. It is
	// reported rather than ignored: an unreadable chunk means verification is
	// unavailable, which must never be confused with verification passing.
	unparsed int64
	// minSectionY is the world's lowest section, needed to place a section's
	// blocks at real Y coordinates. Zero value means "not yet known".
	minSectionY int32
	haveMinY    bool
}

// newBlockLedger collects the positions a trace touches. Returns nil when the
// trace touches none, so sessions that never dig pay nothing.
func newBlockLedger(tr *tracefile.Trace, agg *Aggregate) *blockLedger {
	want := map[[3]int32]bool{}
	for _, e := range tr.Events {
		switch e.Kind {
		case rawevent.KindDig:
			if d, err := rawevent.DecodeDig(e.Data); err == nil {
				want[[3]int32{d.X, d.Y, d.Z}] = true
			}
		case rawevent.KindPlaceBlock:
			if p, err := rawevent.DecodePlace(e.Data); err == nil {
				dx, dy, dz := faceOffset(p.Face)
				want[[3]int32{p.X + dx, p.Y + dy, p.Z + dz}] = true
			}
		}
	}
	if len(want) == 0 {
		return nil
	}
	return &blockLedger{want: want, agg: agg, state: make(map[[3]int32]int32, len(want))}
}

// wants reports whether a position is one this ledger tracks.
func (l *blockLedger) wants(x, y, z int32) bool {
	return l.want[[3]int32{x, y, z}]
}

// setMinSectionY records the world's vertical origin, derived from how many
// sections a chunk column carries.
//
// The client is told the world height in the registry data it currently
// ignores, so it is inferred from the section count instead — 24 sections is a
// 384-block world starting at -64, 16 is a 256-block one starting at 0. A
// datapack can define something else, and rather than guess, an unrecognised
// height leaves the ledger empty so every dig reports as unverified instead of
// as verified against blocks read at the wrong height.
func (l *blockLedger) setMinSectionY(sections int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.haveMinY {
		return true
	}
	switch sections {
	case 24:
		l.minSectionY = -4
	case 16:
		l.minSectionY = 0
	default:
		return false
	}
	l.haveMinY = true
	return true
}

func (l *blockLedger) minY() (int32, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.minSectionY, l.haveMinY
}

// applyChunk records the states of tracked positions inside a chunk column.
func (l *blockLedger) applyChunk(c mcproto.ChunkBlocks) {
	if len(c.States) == 0 {
		return
	}
	l.mu.Lock()
	for p, s := range c.States {
		l.state[p] = s
	}
	l.mu.Unlock()
}

// set records a single block change.
func (l *blockLedger) set(x, y, z, state int32) {
	if !l.wants(x, y, z) {
		return
	}
	l.mu.Lock()
	l.state[[3]int32{x, y, z}] = state
	l.mu.Unlock()
}

// lookup returns the last known state at a position.
func (l *blockLedger) lookup(x, y, z int32) (int32, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s, ok := l.state[[3]int32{x, y, z}]
	return s, ok
}

func (l *blockLedger) noteUnparsed() {
	l.mu.Lock()
	l.unparsed++
	l.mu.Unlock()
	if l.agg != nil {
		l.agg.ChunksUnparsed.Add(1)
	}
}

func (l *blockLedger) unparsedChunks() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.unparsed
}
