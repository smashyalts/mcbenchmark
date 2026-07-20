package replay

import (
	"net"
	"sync"
	"testing"
	"time"

	"mcbench/internal/mcproto"
	"mcbench/internal/rawevent"
	"mcbench/internal/tracefile"
)

// mockServer implements just enough of the server side of the 1.21.4 protocol
// to bring a replay session to play state and observe its movement packets.
type mockResult struct {
	reachedPlay   bool
	teleportOK    bool
	movePackets   int
	flyingPackets int
	keepAliveEcho bool
	loginName     string
	digStatuses   []int32
	err           string
}

func runMockServer(t *testing.T, ln net.Listener, useCompression bool, out *mockResult, done chan<- struct{}) {
	defer close(done)
	conn, err := ln.Accept()
	if err != nil {
		out.err = "accept: " + err.Error()
		return
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	c := mcproto.NewCodec(conn)

	// Handshake.
	if _, _, err := c.ReadPacket(); err != nil {
		out.err = "read handshake: " + err.Error()
		return
	}
	// Login start.
	_, body, err := c.ReadPacket()
	if err != nil {
		out.err = "read login start: " + err.Error()
		return
	}
	out.loginName, _ = mcwireString(body)

	if useCompression {
		// set_compression threshold=64
		if err := c.WritePacket(mcproto.CBLoginSetCompression, varint(64)); err != nil {
			out.err = "send compression: " + err.Error()
			return
		}
		c.EnableCompression(64)
	}
	// login_success: uuid(16) + name + 0 properties. Client ignores contents.
	success := make([]byte, 16)
	success = append(success, appendString(nil, out.loginName)...)
	success = append(success, 0) // property count varint 0
	if err := c.WritePacket(mcproto.CBLoginSuccess, success); err != nil {
		out.err = "send login success: " + err.Error()
		return
	}
	// login_acknowledged
	if _, _, err := c.ReadPacket(); err != nil {
		out.err = "read login ack: " + err.Error()
		return
	}

	// Configuration: send finish immediately.
	if err := c.WritePacket(mcproto.CBConfigFinish, nil); err != nil {
		out.err = "send config finish: " + err.Error()
		return
	}
	if _, _, err := c.ReadPacket(); err != nil { // finish ack
		out.err = "read config finish ack: " + err.Error()
		return
	}

	// Play: sync position with teleport id 42.
	sp := appendVarInt(nil, 42)
	sp = appendF64(sp, 0, 64, 0) // x,y,z
	sp = appendF64(sp, 0, 0, 0)  // velocity
	sp = appendF32(sp, 0, 0)     // yaw, pitch
	sp = append(sp, 0, 0, 0, 0)  // flags int32 = 0 (absolute)
	if err := c.WritePacket(mcproto.CBPlaySyncPosition, sp); err != nil {
		out.err = "send sync position: " + err.Error()
		return
	}
	out.reachedPlay = true

	// Send a keep-alive and expect an echo.
	sentKeepAlive := false
	for {
		id, body, err := c.ReadPacket()
		if err != nil {
			return // client closed / deadline: end of test window
		}
		switch id {
		case mcproto.SBPlayTeleportConfirm:
			out.teleportOK = true
			// Now probe keep-alive.
			if !sentKeepAlive {
				sentKeepAlive = true
				_ = c.WritePacket(mcproto.CBPlayKeepAlive, mcproto.KeepAlive(0x1234))
			}
		case mcproto.SBPlayPositionLook:
			out.movePackets++
		case mcproto.SBPlayFlying:
			out.flyingPackets++
		case mcproto.SBPlayBlockDig:
			if st, err := mcwireVarInt(body); err == nil {
				out.digStatuses = append(out.digStatuses, st)
			}
		case mcproto.SBPlayKeepAlive:
			if id64, _ := mcproto.ParseKeepAlive(body); id64 == 0x1234 {
				out.keepAliveEcho = true
			}
		}
	}
}

func newTestSession(target, host string, port uint16, tr *tracefile.Trace, coll *Collector) *Session {
	return &Session{
		ID: "test", Username: "BENCH_00001", Trace: tr,
		Target: target, Host: host, Port: port, Protocol: mcproto.ProtocolDefault,
		PlayFor: 700 * time.Millisecond, agg: &coll.Agg, coll: coll,
	}
}

func TestReplaySessionFlow(t *testing.T) {
	for _, compress := range []bool{false, true} {
		name := "plain"
		if compress {
			name = "compressed"
		}
		t.Run(name, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			addr := ln.Addr().(*net.TCPAddr)

			var res mockResult
			done := make(chan struct{})
			go runMockServer(t, ln, compress, &res, done)

			tr := &tracefile.Trace{
				SchemaVersion: tracefile.SchemaVersion, ProtocolVersion: mcproto.ProtocolDefault,
				WorldProfileID: "test", TraceID: "t1", DurationUs: 300_000,
				Events: []tracefile.TraceEvent{
					{OffsetUs: 0, Kind: rawevent.KindMove,
						Data: rawevent.MovePayload{DX: 0.1, DZ: 0.1, Yaw: 5, OnGround: true}.Encode()},
					{OffsetUs: 100_000, Kind: rawevent.KindMove,
						Data: rawevent.MovePayload{DX: 0.1, DZ: 0.1, Yaw: 10, OnGround: true}.Encode()},
					{OffsetUs: 200_000, Kind: rawevent.KindSprintToggle, Data: rawevent.EncodeToggle(true)},
				},
			}
			coll := NewCollector()
			sess := newTestSession(addr.String(), "127.0.0.1", uint16(addr.Port), tr, coll)

			stop := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(1)
			go func() { defer wg.Done(); sess.Run(stop) }()
			wg.Wait()
			close(stop)
			<-done

			if res.err != "" {
				t.Fatalf("mock server error: %s", res.err)
			}
			if res.loginName != "BENCH_00001" {
				t.Errorf("login name = %q", res.loginName)
			}
			if !res.reachedPlay {
				t.Error("session did not reach play state")
			}
			if !res.teleportOK {
				t.Error("no teleport confirm received")
			}
			if res.movePackets < 2 {
				t.Errorf("expected >=2 movement packets, got %d", res.movePackets)
			}
			if !res.keepAliveEcho {
				t.Error("keep-alive was not echoed")
			}
			if coll.Agg.Connected.Load() != 1 {
				t.Errorf("connected=%d", coll.Agg.Connected.Load())
			}
		})
	}
}

// TestClientSendsMovementEveryTick pins the packet rate a session offers.
//
// A real client sends exactly one movement packet per tick whether or not it
// moved. The generator used to send movement only when the trace had a movement
// event, which measured 7.2 events/sec/player against the ~20/sec a real client
// produces — so the server's packet-handling path, the thing this benchmark
// exists to load, saw about a third of its true traffic. The trace below has a
// single movement event, so almost every packet counted here is an idle one.
func TestClientSendsMovementEveryTick(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().(*net.TCPAddr)

	var res mockResult
	done := make(chan struct{})
	go runMockServer(t, ln, false, &res, done)

	tr := &tracefile.Trace{
		SchemaVersion: tracefile.SchemaVersion, ProtocolVersion: mcproto.ProtocolDefault,
		WorldProfileID: "test", TraceID: "idle", DurationUs: 600_000,
		Events: []tracefile.TraceEvent{
			{OffsetUs: 0, Kind: rawevent.KindMove,
				Data: rawevent.MovePayload{DX: 0.1, OnGround: true}.Encode()},
		},
	}
	coll := NewCollector()
	sess := newTestSession(addr.String(), "127.0.0.1", uint16(addr.Port), tr, coll)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); sess.Run(stop) }()
	wg.Wait()
	close(stop)
	<-done

	total := res.movePackets + res.flyingPackets
	// PlayFor is 700 ms => ~14 ticks. Allow slack for connection setup and
	// scheduling, but a regression to event-only movement would give ~1.
	if total < 8 {
		t.Errorf("expected ~one movement packet per tick (>=8 in 700ms), got %d "+
			"(pos_look=%d flying=%d)", total, res.movePackets, res.flyingPackets)
	}
	// The stationary ticks must be status-only packets, not full positions.
	if res.flyingPackets == 0 {
		t.Error("no status-only flying packets sent on idle ticks")
	}
}

// TestDigSendsStartBeforeFinish pins the dig packet sequence.
//
// Capture sees only the end of a dig (BlockBreakEvent fires once the block is
// already gone), so every captured dig carries action=finish. The replay used to
// forward that finish alone, and the vanilla server drops a stop it never saw a
// start for — the bot swung at the block forever and nothing broke, so a trace
// full of mining produced none of the block-update, drop-spawn or chunk-save
// load it was supposed to.
func TestDigSendsStartBeforeFinish(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().(*net.TCPAddr)

	var res mockResult
	done := make(chan struct{})
	go runMockServer(t, ln, false, &res, done)

	tr := &tracefile.Trace{
		SchemaVersion: tracefile.SchemaVersion, ProtocolVersion: mcproto.ProtocolDefault,
		WorldProfileID: "test", TraceID: "dig", DurationUs: 300_000,
		Events: []tracefile.TraceEvent{
			{OffsetUs: 100_000, Kind: rawevent.KindDig,
				Data: rawevent.DigPayload{Action: mcproto.DigFinish,
					X: 10, Y: 64, Z: -5, Face: 1}.Encode()},
		},
	}
	coll := NewCollector()
	sess := newTestSession(addr.String(), "127.0.0.1", uint16(addr.Port), tr, coll)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); sess.Run(stop) }()
	wg.Wait()
	close(stop)
	<-done

	if res.err != "" {
		t.Fatalf("mock server error: %s", res.err)
	}
	want := []int32{mcproto.DigStart, mcproto.DigFinish}
	if len(res.digStatuses) != len(want) {
		t.Fatalf("dig statuses = %v, want %v", res.digStatuses, want)
	}
	for i, w := range want {
		if res.digStatuses[i] != w {
			t.Errorf("dig status[%d] = %d, want %d", i, res.digStatuses[i], w)
		}
	}
}

// TestReanchorNearIsAdopted covers a small server-side relocation: close enough
// that the server accepts the bot claiming it, so the view follows outright
// rather than accumulating a delta.
func TestReanchorNearIsAdopted(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().(*net.TCPAddr)

	var res mockResult
	done := make(chan struct{})
	go runMockServer(t, ln, false, &res, done)

	// Spawn is (0,64,0); jump to (3,66,-4), well inside the self-move limit.
	tr := &tracefile.Trace{
		SchemaVersion: tracefile.SchemaVersion, ProtocolVersion: mcproto.ProtocolDefault,
		WorldProfileID: "test", TraceID: "near", DurationUs: 300_000,
		Events: []tracefile.TraceEvent{
			{OffsetUs: 100_000, Kind: rawevent.KindReanchor,
				Data: rawevent.ReanchorPayload{X: 3, Y: 66, Z: -4, Yaw: 90}.Encode()},
		},
	}
	coll := NewCollector()
	sess := newTestSession(addr.String(), "127.0.0.1", uint16(addr.Port), tr, coll)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); sess.Run(stop) }()
	wg.Wait()
	close(stop)
	<-done

	sess.viewMu.Lock()
	got := sess.view
	sess.viewMu.Unlock()
	if got.X != 3 || got.Y != 66 || got.Z != -4 {
		t.Errorf("view = (%.1f,%.1f,%.1f), want (3,66,-4) applied absolutely",
			got.X, got.Y, got.Z)
	}
	if n := coll.Agg.RelocationsUnreproduced.Load(); n != 0 {
		t.Errorf("relocations_unreproduced = %d, want 0", n)
	}
	if n := coll.Agg.EventsSkipped.Load(); n != 0 {
		t.Errorf("events skipped = %d, want 0 (re-anchor must be handled)", n)
	}
}

// TestReanchorFarIsCountedNotFaked covers a real teleport.
//
// A client cannot teleport itself. Claiming a position 1600 blocks away is
// exactly what an illegal move looks like, so the server rejects it and
// rubber-bands -- verified on Paper 26.1.2, where a replayed 1700-block teleport
// moved the bot nowhere. Faking it would add packets the server throws away and
// leave the view disagreeing with reality, so the run reports the divergence
// instead.
func TestReanchorFarIsCountedNotFaked(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().(*net.TCPAddr)

	var res mockResult
	done := make(chan struct{})
	go runMockServer(t, ln, false, &res, done)

	tr := &tracefile.Trace{
		SchemaVersion: tracefile.SchemaVersion, ProtocolVersion: mcproto.ProtocolDefault,
		WorldProfileID: "test", TraceID: "far", DurationUs: 300_000,
		Events: []tracefile.TraceEvent{
			{OffsetUs: 100_000, Kind: rawevent.KindReanchor,
				Data: rawevent.ReanchorPayload{X: 1600.5, Y: 72, Z: -800.5}.Encode()},
		},
	}
	coll := NewCollector()
	sess := newTestSession(addr.String(), "127.0.0.1", uint16(addr.Port), tr, coll)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); sess.Run(stop) }()
	wg.Wait()
	close(stop)
	<-done

	sess.viewMu.Lock()
	got := sess.view
	sess.viewMu.Unlock()
	if got.X > 100 || got.Z < -100 {
		t.Errorf("view = (%.1f,%.1f,%.1f); an unreachable teleport must not be "+
			"claimed, the server would reject it", got.X, got.Y, got.Z)
	}
	if n := coll.Agg.RelocationsUnreproduced.Load(); n != 1 {
		t.Errorf("relocations_unreproduced = %d, want 1", n)
	}
}
