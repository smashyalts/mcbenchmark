package replay

import (
	"errors"
	"fmt"
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

	dcReason string
	dcOnce   sync.Once
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
	err := s.codec.WritePacket(id, body)
	if err == nil {
		atomic.AddInt64(&s.packetsSent, 1)
		s.agg.PacketsSent.Add(1)
	}
	return err
}
