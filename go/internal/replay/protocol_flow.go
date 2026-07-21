package replay

import (
	"fmt"
	"time"

	"mcbench/internal/mcproto"
)

// vanillaResyncTicks is how often a stationary client sends a full position
// packet instead of a status-only one. The vanilla client forces a positional
// re-sync every 20 ticks (once a second) even when standing perfectly still.
const vanillaResyncTicks = 20

// handshakeAndLogin performs handshake + login handshake up to the point where
// the connection enters the configuration phase. It runs synchronously (no
// concurrent reader yet).
func (s *Session) handshakeAndLogin() error {
	s.setState(StateLogin)
	if err := s.send(mcproto.SBHandshake,
		mcproto.Handshake(s.Protocol, s.Host, s.Port, mcproto.IntentLogin)); err != nil {
		return fmt.Errorf("send handshake: %w", err)
	}
	uuid := mcproto.OfflineUUID(s.Username)
	if err := s.send(mcproto.SBLoginStart, mcproto.LoginStart(s.Username, uuid)); err != nil {
		return fmt.Errorf("send login start: %w", err)
	}

	for {
		id, body, err := s.codec.ReadPacket()
		if err != nil {
			return fmt.Errorf("login read: %w", err)
		}
		switch id {
		case mcproto.CBLoginSetCompression:
			th, err := mcwireVarInt(body)
			if err != nil {
				return err
			}
			s.codec.EnableCompression(int(th))
		case mcproto.CBLoginEncryptionRequest:
			return fmt.Errorf("server is in online mode (sent encryption request); benchmark server must be offline-mode")
		case mcproto.CBLoginSuccess:
			if err := s.send(mcproto.SBLoginAcknowledged, nil); err != nil {
				return fmt.Errorf("send login ack: %w", err)
			}
			s.setState(StateConfiguration)
			return nil
		case mcproto.CBLoginDisconnect:
			return fmt.Errorf("login disconnect: %s", decodeText(body))
		case mcproto.CBLoginPluginRequest:
			// Respond "not understood": message id + boolean false.
			if err := s.replyPluginRequest(body); err != nil {
				return err
			}
		case mcproto.CBLoginCookieRequest:
			// Not supported by offline vanilla flow; ignore.
		default:
			return fmt.Errorf("unexpected login packet 0x%02X", id)
		}
	}
}

// readLoop consumes packets for the configuration and play phases until the
// connection closes. onPlayReady is called once when the play position handshake
// completes.
func (s *Session) readLoop(onPlayReady func()) {
	inConfig := true
	for {
		id, body, err := s.codec.ReadPacket()
		if err != nil {
			s.fail("read: " + err.Error())
			return
		}
		if inConfig {
			done, err := s.handleConfig(id, body)
			if err != nil {
				s.fail(err.Error())
				return
			}
			if done {
				inConfig = false
				s.setState(StatePlayAwaitingPosition)
			}
			continue
		}
		if s.handlePlay(id, body, onPlayReady, &inConfig) {
			return
		}
	}
}

// handleConfig processes a configuration-phase packet; returns true when the
// server signals finish_configuration.
func (s *Session) handleConfig(id int32, body []byte) (bool, error) {
	switch id {
	case mcproto.CBConfigKeepAlive:
		kid, err := mcproto.ParseKeepAlive(body)
		if err != nil {
			return false, err
		}
		return false, s.send(mcproto.SBConfigKeepAlive, mcproto.KeepAlive(kid))
	case mcproto.CBConfigPing:
		pid, err := mcproto.ParsePing(body)
		if err != nil {
			return false, err
		}
		return false, s.send(mcproto.SBConfigPong, mcproto.Pong(pid))
	case mcproto.CBConfigKnownPacks:
		// Reply with our client information first (some servers expect it),
		// then acknowledge with empty known packs.
		_ = s.send(mcproto.SBConfigClientInformation, mcproto.ClientInformation())
		return false, s.send(mcproto.SBConfigKnownPacks, mcproto.KnownPacksEmpty())
	case mcproto.CBConfigCodeOfConduct:
		// 26.x may require accepting a code of conduct before finishing config.
		return false, s.send(mcproto.SBConfigAcceptCoC, nil)
	case mcproto.CBConfigFinish:
		if err := s.send(mcproto.SBConfigFinishAck, nil); err != nil {
			return false, err
		}
		return true, nil
	case mcproto.CBConfigDisconnect:
		return false, fmt.Errorf("config disconnect: %s", decodeText(body))
	case mcproto.CBConfigPluginMessage, mcproto.CBConfigRegistryData, mcproto.CBConfigCookieRequest:
		// Registry data, tags, feature flags, etc. — no reply required.
		return false, nil
	default:
		// Unknown configuration packets are safe to ignore.
		return false, nil
	}
}

// handlePlay processes a play-phase packet. Returns true if the session should
// end (disconnect/error). It flips *inConfig back to true if the server sends
// start_configuration.
func (s *Session) handlePlay(id int32, body []byte, onPlayReady func(), inConfig *bool) bool {
	switch id {
	case mcproto.CBPlaySyncPosition:
		p, err := mcproto.ParseSyncPosition(body)
		if err != nil {
			s.fail("sync position: " + err.Error())
			return true
		}
		s.viewMu.Lock()
		// Only the first sync_position is the spawn. Later ones are the server
		// correcting a move it rejected, and a bot that legitimately walked away
		// from its origin must not be reported as misplaced.
		firstSync := !s.view.HasPos
		s.applyTeleport(p)
		s.view.HasPos = true
		x, y, z, yaw, pitch := s.view.X, s.view.Y, s.view.Z, s.view.Yaw, s.view.Pitch
		s.viewMu.Unlock()
		if firstSync {
			s.checkSpawnAgainstOrigin(x, y, z)
		}
		if err := s.send(mcproto.SBPlayTeleportConfirm, mcproto.TeleportConfirm(p.TeleportID)); err != nil {
			s.fail("teleport confirm: " + err.Error())
			return true
		}
		// Confirm our post-teleport position so the server accepts movement.
		_ = s.send(mcproto.SBPlayPositionLook,
			mcproto.PositionLook(x, y, z, yaw, pitch, true))
		onPlayReady()
	case mcproto.CBPlayAddEntity:
		// What the client is told exists nearby. Attacks are aimed with this.
		if a, err := mcproto.ParseAddEntity(body); err == nil && s.entities != nil {
			s.entities.add(a)
		}
	case mcproto.CBPlayRemoveEntities:
		if ids, err := mcproto.ParseRemoveEntities(body); err == nil && s.entities != nil {
			s.entities.remove(ids)
		}
	case mcproto.CBPlayLevelChunk:
		s.readChunk(body)
	case mcproto.CBPlaySectionBlocksUpdate:
		if changes, err := mcproto.ParseSectionBlocksUpdate(body); err == nil && s.blocks != nil {
			for p, st := range changes {
				s.blocks.set(p[0], p[1], p[2], st)
			}
		}
	case mcproto.CBPlayBlockUpdate:
		// The server's verdict on a dig. Without this the run can only report
		// that packets were sent, which is exactly the failure mode that makes a
		// replay look successful while the world never changes.
		if b, err := mcproto.ParseBlockUpdate(body); err == nil {
			s.confirmBlockUpdate(b)
		}
	case mcproto.CBPlayKeepAlive:
		kid, err := mcproto.ParseKeepAlive(body)
		if err != nil {
			s.fail("play keepalive: " + err.Error())
			return true
		}
		if err := s.send(mcproto.SBPlayKeepAlive, mcproto.KeepAlive(kid)); err != nil {
			s.fail("send keepalive: " + err.Error())
			return true
		}
	case mcproto.CBPlayPing:
		pid, err := mcproto.ParsePing(body)
		if err == nil {
			_ = s.send(mcproto.SBPlayPong, mcproto.Pong(pid))
		}
	case mcproto.CBPlayChunkBatchFinished:
		// Acknowledge so the server keeps streaming chunks.
		_ = s.send(mcproto.SBPlayChunkBatchReceived, mcproto.ChunkBatchReceived(16))
	case mcproto.CBPlayOpenScreen:
		// The server assigns the window id here; capture it live for clicks.
		if o, err := mcproto.ParseOpenScreen(body); err == nil {
			s.curWindow.Store(o.WindowID)
		}
	case mcproto.CBPlayContainerSetContent, mcproto.CBPlayContainerSetSlot:
		// Track the latest state id the click packet must echo. Window id comes
		// only from open_screen/close, so we don't overwrite it here.
		if _, st, err := mcproto.ParseContainerStateID(body); err == nil {
			s.curState.Store(st)
		}
	case mcproto.CBPlayContainerClose:
		// Server closed the window; fall back to the player inventory.
		s.curWindow.Store(0)
	case mcproto.CBPlayStartConfiguration:
		_ = s.send(mcproto.SBPlayConfigurationAck, nil)
		*inConfig = true
		s.setState(StateConfiguration)
	case mcproto.CBPlayDisconnect:
		s.fail("play disconnect: " + decodeText(body))
		return true
	}
	return false
}

// readChunk folds a chunk column into the block ledger.
//
// Only the positions the trace touches are kept — see blockLedger — but the
// column still has to be walked in full to reach them, because chunk sections
// are variable-length and there is nothing to seek by.
func (s *Session) readChunk(body []byte) {
	if s.blocks == nil {
		return // this trace touches no blocks; nothing to verify
	}
	minY, known := s.blocks.minY()
	if !known {
		// The world's vertical origin is not in this packet, and the registry
		// data that carries it is not parsed. Infer it from the column height,
		// which is the same for every column, then use it for the rest of the run.
		n, err := mcproto.CountChunkSections(body)
		if err != nil || !s.blocks.setMinSectionY(n) {
			s.blocks.noteUnparsed()
			return
		}
		minY, _ = s.blocks.minY()
	}
	c, err := mcproto.ParseChunkBlocks(body, minY, s.blocks.wants)
	if err != nil {
		// The chunk format has moved. Say so by counting it: a dig that cannot
		// be verified must not be reported as verified.
		s.blocks.noteUnparsed()
		return
	}
	s.blocks.applyChunk(c)
}

// applyTeleport folds a server teleport into the local view. Callers must hold
// s.viewMu — the reader goroutine races the dispatcher otherwise.
func (s *Session) applyTeleport(p mcproto.SyncPosition) {
	if p.Flags&mcproto.FlagRelX != 0 {
		s.view.X += p.X
	} else {
		s.view.X = p.X
	}
	if p.Flags&mcproto.FlagRelY != 0 {
		s.view.Y += p.Y
	} else {
		s.view.Y = p.Y
	}
	if p.Flags&mcproto.FlagRelZ != 0 {
		s.view.Z += p.Z
	} else {
		s.view.Z = p.Z
	}
	if p.Flags&mcproto.FlagRelYaw != 0 {
		s.view.Yaw += p.Yaw
	} else {
		s.view.Yaw = p.Yaw
	}
	if p.Flags&mcproto.FlagRelPitch != 0 {
		s.view.Pitch += p.Pitch
	} else {
		s.view.Pitch = p.Pitch
	}
}

// replayLoop plays trace events on schedule, looping the trace to fill PlayFor.
func (s *Session) replayLoop(stop <-chan struct{}, readerDone <-chan struct{}) {
	deadline := time.Now().Add(s.PlayFor)
	// Tell the server the player has finished loading (protocol 775).
	_ = s.send(mcproto.SBPlayPlayerLoaded, nil)
	if s.EnableFlight {
		// Creative demo: enable flight so airborne travel is accepted and the
		// server pages in new chunks along the path.
		_ = s.send(mcproto.SBPlayAbilities, mcproto.Abilities(true))
	}

	for time.Now().Before(deadline) {
		loopStart := time.Now()
		if s.playOnce(loopStart, deadline, stop, readerDone) {
			return // ended early (stop or disconnect)
		}
		s.loops++
		if s.PlayFor <= 0 || !s.LoopTrace {
			// Single pass (reuse_policy "once"): stay connected briefly so the
			// server can finish processing the final events (e.g. an auction
			// purchase) before we disconnect. Measured from now — the trace is
			// usually longer than the grace period, so anchoring to loopStart
			// would put the deadline in the past and skip the wait entirely.
			if !s.LoopTrace {
				s.holdUntil(time.Now().Add(2*time.Second), deadline, stop, readerDone)
			}
			return
		}
	}
}

// elapsedInTrace returns microseconds since this pass began.
func (s *Session) elapsedInTrace(loopStart time.Time) int64 {
	return time.Since(loopStart).Microseconds()
}

// holdUntil keeps the connection alive until t, the deadline, or an early
// stop/disconnect.
//
// "Alive" means behaving like a client, not merely leaving the socket open: it
// keeps sending one movement packet per tick. A real player waiting for an
// auction to settle is still streaming position updates, and that traffic is
// part of the load being measured.
func (s *Session) holdUntil(t, deadline time.Time, stop, readerDone <-chan struct{}) {
	if t.After(deadline) {
		t = deadline
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	// One timer for the whole wait. time.After inside the select would arm a
	// fresh one every tick, each held alive until its own full deadline — 20 per
	// second per session, and none of them collectable early.
	done := time.NewTimer(time.Until(t))
	defer done.Stop()
	for {
		select {
		case <-done.C:
			return
		case <-stop:
			return
		case <-readerDone:
			return
		case <-ticker.C:
			if time.Now().After(t) {
				return
			}
			s.sendIdleMovement()
		}
	}
}

// playOnce replays the trace once. Returns true if the session ended early.
func (s *Session) playOnce(loopStart, deadline time.Time, stop <-chan struct{}, readerDone <-chan struct{}) bool {
	ticker := time.NewTicker(50 * time.Millisecond) // 20 Hz, matches server tick
	defer ticker.Stop()
	idx := 0
	events := s.Trace.Events
	// Run to the trace's full duration, not merely to its last event. A trace
	// whose events stop early still represents a player who stayed connected and
	// kept sending movement for the rest of it; ending at the last event both
	// skips that traffic and replays the trace faster than real time.
	for idx < len(events) || s.elapsedInTrace(loopStart) < s.Trace.DurationUs {
		select {
		case <-stop:
			return true
		case <-readerDone:
			return true
		case now := <-ticker.C:
			if now.After(deadline) {
				return false
			}
			elapsedUs := now.Sub(loopStart).Microseconds()
			s.movedThisTick = false
			for idx < len(events) && events[idx].OffsetUs <= elapsedUs {
				s.dispatch(events[idx])
				idx++
			}
			if !s.movedThisTick {
				s.sendIdleMovement()
			}
			if s.getState() != StatePlayReady {
				return true
			}
		}
	}
	return false
}

// sendIdleMovement emits the movement packet a real client sends on a tick where
// it did not move.
//
// A vanilla client sends exactly one movement packet per tick, always. Standing
// still it sends move_player_status_only (just the flags byte), and it forces a
// full position packet every 20 ticks so the server can re-sync. Without this the
// generator sends movement only when the trace has a movement event — measured at
// 7.2 events/sec/player against the ~20/sec a real client produces, so the
// server's packet-handling path saw roughly a third of its true load.
//
// Called from the tick loop only, so it needs no lock beyond the view read.
func (s *Session) sendIdleMovement() {
	if s.getState() != StatePlayReady {
		return
	}
	s.idleTicks++
	if s.idleTicks%vanillaResyncTicks == 0 {
		s.viewMu.Lock()
		x, y, z, yaw, pitch := s.view.X, s.view.Y, s.view.Z, s.view.Yaw, s.view.Pitch
		s.viewMu.Unlock()
		_ = s.send(mcproto.SBPlayPositionLook,
			mcproto.PositionLook(x, y, z, yaw, pitch, true))
		return
	}
	_ = s.send(mcproto.SBPlayFlying, mcproto.Flying(true))
}
