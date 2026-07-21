package replay

import (
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"mcbench/internal/mcproto"
	"mcbench/internal/tracefile"
)

type State int32

const (
	StateNew State = iota
	StateConnecting
	StateLogin
	StateConfiguration
	StatePlayAwaitingPosition
	StatePlayReady
	StateDraining
	StateDisconnected
	StateFailed
)

func (s State) String() string {
	switch s {
	case StateNew:
		return "new"
	case StateConnecting:
		return "connecting"
	case StateLogin:
		return "login"
	case StateConfiguration:
		return "configuration"
	case StatePlayAwaitingPosition:
		return "play_awaiting_position"
	case StatePlayReady:
		return "play_ready"
	case StateDraining:
		return "draining"
	case StateDisconnected:
		return "disconnected"
	case StateFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// WorldView is the minimal replay-side world state.
type WorldView struct {
	X, Y, Z    float64
	Yaw, Pitch float32
	HasPos     bool
	digSeq     int32 // sequence counter for block actions (dig/place/use)
}

func (w *WorldView) nextSeq() int32 {
	w.digSeq++
	return w.digSeq
}

// nextSeq returns the next block-action sequence number under viewMu.
func (s *Session) nextSeq() int32 {
	s.viewMu.Lock()
	defer s.viewMu.Unlock()
	return s.view.nextSeq()
}

// look returns the current yaw/pitch under viewMu.
func (s *Session) look() (yaw, pitch float32) {
	s.viewMu.Lock()
	defer s.viewMu.Unlock()
	return s.view.Yaw, s.view.Pitch
}

// Session drives one replay client connection.
type Session struct {
	ID       string
	Username string
	Trace    *tracefile.Trace
	Target   string
	Host     string
	Port     uint16
	Protocol int32

	// Reuse policy: how long to keep replaying by looping the trace.
	PlayFor time.Duration

	// EnableFlight sends player_abilities(flying) at play start (creative demos).
	EnableFlight bool

	// Origin is where the trace was captured, used only to warn when the server
	// spawns the bot somewhere else. Nil for traces that carry none.
	Origin *tracefile.Origin

	// LoopTrace replays the trace repeatedly until PlayFor; false (reuse_policy
	// "once") plays it a single time, then the session ends. Ordered scenarios
	// like the auction-house list→buy flow need a single pass.
	LoopTrace bool

	agg  *Aggregate
	coll *Collector

	codec *mcproto.Codec
	state atomic.Int32

	// Per-tick movement bookkeeping, touched only by the replay goroutine.
	// A real client sends exactly one movement packet per tick: whatever the
	// trace supplies, or a status-only packet when the trace supplies nothing.
	movedThisTick bool
	idleTicks     int

	// view is touched by two goroutines: the reader (server teleports, via
	// applyTeleport) and the dispatcher (movement/block sequence numbers), so
	// every access must hold viewMu.
	viewMu sync.Mutex
	view   WorldView

	// Live container state, set by the reader goroutine from server packets and
	// read by the dispatch goroutine — hence atomic. curWindow defaults to 0,
	// the player's own inventory, which is always "open".
	curWindow atomic.Int32
	curState  atomic.Int32

	eventsReplayed int64
	packetsSent    int64
	loops          int

	// Positions this session has dug or built at and not yet seen the server
	// confirm. Written by the dispatch goroutine, read and cleared by the reader
	// goroutine when a block_update arrives, hence the mutex.
	digMu        sync.Mutex
	digPending   map[[3]int32]bool
	placePending map[[3]int32]bool

	// Live entities the server has told this client about, so an attack can be
	// aimed at something real instead of being a bare animation.
	entities *entityTracker

	// digStarted tracks positions this session has already sent a START for, so
	// a trace that carries its own real START is not given a synthetic one too.
	// Touched only by the dispatch goroutine.
	digStarted map[[3]int32]bool

	dcReason      string
	dcOnce        sync.Once
	spawnWarnOnce sync.Once
}

func (s *Session) setState(st State) { s.state.Store(int32(st)) }
func (s *Session) getState() State   { return State(s.state.Load()) }

func (s *Session) fail(reason string) {
	s.dcOnce.Do(func() { s.dcReason = reason })
}

// Run performs the full lifecycle and records a SessionResult. It returns when
// the session ends (disconnect, error, or ctx/deadline).
func (s *Session) Run(stop <-chan struct{}) {
	dialStart := time.Now()
	result := SessionResult{ID: s.ID, Username: s.Username, TraceFile: s.Trace.TraceID}

	conn, err := net.DialTimeout("tcp", s.Target, 10*time.Second)
	if err != nil {
		s.setState(StateFailed)
		s.agg.Failed.Add(1)
		result.State = StateFailed.String()
		result.DisconnectReason = "dial: " + err.Error()
		s.coll.AddSession(result)
		return
	}
	s.codec = mcproto.NewCodec(conn)
	s.setState(StateConnecting)

	if err := s.handshakeAndLogin(); err != nil {
		s.finishFailed(&result, dialStart, err)
		return
	}

	// Reader goroutine owns all packet reads; the main goroutine sends trace
	// events. A channel signals when play state is reached or the session dies.
	playReady := make(chan struct{})
	readerDone := make(chan struct{})
	var readyOnce sync.Once
	go func() {
		defer close(readerDone)
		s.readLoop(func() { readyOnce.Do(func() { close(playReady) }) })
	}()

	select {
	case <-playReady:
	case <-readerDone:
		s.finishFailed(&result, dialStart, fmt.Errorf("disconnected during login/config: %s", s.dcReason))
		return
	case <-time.After(30 * time.Second):
		s.codec.Close()
		<-readerDone
		s.finishFailed(&result, dialStart, errors.New("timeout awaiting play state"))
		return
	case <-stop:
		s.codec.Close()
		<-readerDone
		s.finishFailed(&result, dialStart, errors.New("run stopped before play state"))
		return
	}

	connectMs := time.Since(dialStart).Milliseconds()
	s.agg.Connected.Add(1)
	s.agg.Active.Add(1)
	s.setState(StatePlayReady)
	playStart := time.Now()

	s.replayLoop(stop, readerDone)

	s.agg.Active.Add(-1)
	s.agg.Disconnected.Add(1)
	s.codec.Close()
	<-readerDone

	result.State = s.getState().String()
	result.ConnectMs = connectMs
	result.DurationS = int64(time.Since(playStart).Seconds())
	result.EventsReplayed = atomic.LoadInt64(&s.eventsReplayed)
	result.PacketsSent = atomic.LoadInt64(&s.packetsSent)
	result.TraceLoops = s.loops
	result.DisconnectReason = s.dcReason
	s.coll.AddSession(result)
	s.agg.BytesIn.Add(s.codec.BytesIn)
	s.agg.BytesOut.Add(s.codec.BytesOut)
}

func (s *Session) finishFailed(result *SessionResult, dialStart time.Time, err error) {
	s.setState(StateFailed)
	s.agg.Failed.Add(1)
	if s.codec != nil {
		s.codec.Close()
		s.agg.BytesIn.Add(s.codec.BytesIn)
		s.agg.BytesOut.Add(s.codec.BytesOut)
	}
	result.State = StateFailed.String()
	result.ConnectMs = time.Since(dialStart).Milliseconds()
	if result.DisconnectReason == "" {
		result.DisconnectReason = err.Error()
	}
	s.coll.AddSession(*result)
}

func (s *Session) send(id int32, body []byte) error {
	if s.codec == nil {
		// No connection: a session that failed to dial, or a unit test driving
		// dispatch directly to check its bookkeeping. Either way, dropping the
		// packet is right and panicking is not.
		return net.ErrClosed
	}
	err := s.codec.WritePacket(id, body)
	if err == nil {
		atomic.AddInt64(&s.packetsSent, 1)
		s.agg.PacketsSent.Add(1)
	}
	return err
}

// noteDig records a position this session asked the server to break, so the
// reader can tell whether the server actually did it.
//
// Sending a dig packet is not evidence the block broke. The server silently
// drops one it considers out of range, or for a block that is already air, and
// a run that counts "events replayed" alone reports total success either way.
// That has now been the wrong answer twice, so the client checks.
func (s *Session) noteDig(x, y, z int32) {
	s.digMu.Lock()
	if s.digPending == nil {
		s.digPending = make(map[[3]int32]bool)
	}
	s.digPending[[3]int32{x, y, z}] = true
	s.digMu.Unlock()
	s.agg.DigsSent.Add(1)
}

// notePlace records where a placement is expected to land.
//
// The packet carries the block that was clicked and which face of it, so the
// new block goes one step along that face — the same arithmetic the server
// does. Worth tracking for the same reason as digs, and for a sharper one: a
// placement aimed at the wrong position is not rejected loudly, it simply does
// nothing, and the run reports the event as replayed either way.
func (s *Session) notePlace(x, y, z, face int32) {
	dx, dy, dz := faceOffset(face)
	s.digMu.Lock()
	if s.placePending == nil {
		s.placePending = make(map[[3]int32]bool)
	}
	s.placePending[[3]int32{x + dx, y + dy, z + dz}] = true
	s.digMu.Unlock()
	s.agg.PlacesSent.Add(1)
}

// faceOffset maps a protocol block face to the direction it points.
func faceOffset(face int32) (x, y, z int32) {
	switch face {
	case 0:
		return 0, -1, 0
	case 1:
		return 0, 1, 0
	case 2:
		return 0, 0, -1
	case 3:
		return 0, 0, 1
	case 4:
		return -1, 0, 0
	case 5:
		return 1, 0, 0
	}
	return 0, 0, 0
}

// confirmBlockUpdate matches a clientbound block_update against the digs and
// placements we sent: air where we dug, anything else where we built.
func (s *Session) confirmBlockUpdate(b mcproto.BlockUpdate) {
	key := [3]int32{b.X, b.Y, b.Z}
	s.digMu.Lock()
	var dug, built bool
	if b.StateID == mcproto.AirStateID {
		if dug = s.digPending[key]; dug {
			delete(s.digPending, key)
		}
	} else if built = s.placePending[key]; built {
		delete(s.placePending, key)
	}
	s.digMu.Unlock()
	if dug {
		s.agg.DigsConfirmed.Add(1)
	}
	if built {
		s.agg.PlacesConfirmed.Add(1)
	}
}

// checkSpawnAgainstOrigin warns when the server put the bot somewhere other
// than where its trace was captured.
//
// This is the single most common reason a replay looks like it worked and
// changed nothing: the account had no player data, so it spawned at world spawn,
// out of interaction range of every block the trace touches. It is invisible
// from the run report, so say it plainly, once, at the point it is known.
func (s *Session) checkSpawnAgainstOrigin(x, y, z float64) {
	if s.Origin == nil {
		return
	}
	dx, dy, dz := x-s.Origin.X, y-s.Origin.Y, z-s.Origin.Z
	if dx*dx+dy*dy+dz*dz <= originWarnDistSq {
		return
	}
	s.spawnWarnOnce.Do(func() {
		log.Printf("WARNING: %s spawned at (%.1f, %.1f, %.1f) but its trace was "+
			"captured at (%.1f, %.1f, %.1f), %.0f blocks away. Block events will be "+
			"out of range and do nothing. Run bench-playerdata against this server's "+
			"world with the server stopped, before every run.",
			s.Username, x, y, z, s.Origin.X, s.Origin.Y, s.Origin.Z,
			math.Sqrt(dx*dx+dy*dy+dz*dz))
	})
}

// originWarnDistSq is the squared distance beyond which a spawn is reported as
// misplaced. Interaction range is about 4.5 blocks, so 16 (4 blocks) is inside
// it: anything further and the trace's block events are already unreliable.
const originWarnDistSq = 16.0

// chatClock is the timestamp a chat packet carries.
//
// The server uses it only to reject messages from the future or the distant
// past, so wall-clock now is both correct and the only thing it can be: the
// capture's timestamp would be hours stale by replay time and rejected.
func (s *Session) chatClock() int64 { return time.Now().UnixMilli() }
