package replay

import (
	"strings"
	"sync/atomic"

	"mcbench/internal/mcproto"
	"mcbench/internal/mcwire"
	"mcbench/internal/rawevent"
	"mcbench/internal/tracefile"
)

// maxSelfMoveSq is the squared distance a client may put between two of its own
// position packets before the server calls it cheating. Vanilla and Paper reject
// a move whose squared length exceeds 100 (10 blocks) and teleport the player
// back; 64 leaves margin under that so a re-anchor we do adopt is never itself
// the thing that trips the check.
const maxSelfMoveSq = 64.0

// dispatch converts one trace event into serverbound packet(s) and updates the
// local world view. Unmapped kinds increment EventsSkipped.
func (s *Session) dispatch(e tracefile.TraceEvent) {
	handled := true
	switch e.Kind {
	case rawevent.KindMove:
		m, err := rawevent.DecodeMove(e.Data)
		if err != nil {
			handled = false
			break
		}
		s.viewMu.Lock()
		s.view.X += float64(m.DX)
		s.view.Y += float64(m.DY)
		s.view.Z += float64(m.DZ)
		s.view.Yaw = m.Yaw
		s.view.Pitch = m.Pitch
		x, y, z, yaw, pitch := s.view.X, s.view.Y, s.view.Z, s.view.Yaw, s.view.Pitch
		s.viewMu.Unlock()
		_ = s.send(mcproto.SBPlayPositionLook,
			mcproto.PositionLook(x, y, z, yaw, pitch, m.OnGround))
		// This tick's movement packet is accounted for, so the tick loop must
		// not also send an idle one — a real client sends exactly one.
		s.movedThisTick = true
		s.idleTicks = 0

	case rawevent.KindReanchor:
		// The server relocated the captured player, so the delta chain restarts
		// here.
		//
		// What replay can do about it is limited, and pretending otherwise makes
		// things worse. A client cannot teleport itself: claiming a position 1600
		// blocks away is indistinguishable from cheating, so the server rejects
		// it and rubber-bands the bot straight back. Measured on Paper 26.1.2 —
		// a replayed 1700-block teleport moved the bot nowhere at all.
		//
		// So only adopt the position when it is close enough that the server will
		// accept it as ordinary movement, which covers the small corrections
		// plugins make. For a real teleport the bot follows only if the benchmark
		// server teleports it too — because the captured command replayed, or it
		// walked into the same portal — and that arrives as a server
		// sync_position, which the reader already folds into the view. Anything
		// left over is counted rather than faked.
		a, err := rawevent.DecodeReanchor(e.Data)
		if err != nil {
			handled = false
			break
		}
		s.viewMu.Lock()
		dx, dy, dz := a.X-s.view.X, a.Y-s.view.Y, a.Z-s.view.Z
		near := dx*dx+dy*dy+dz*dz <= maxSelfMoveSq
		if near {
			s.view.X, s.view.Y, s.view.Z = a.X, a.Y, a.Z
			s.view.Yaw, s.view.Pitch = a.Yaw, a.Pitch
		}
		s.viewMu.Unlock()
		if !near {
			s.agg.RelocationsUnreproduced.Add(1)
			break
		}
		_ = s.send(mcproto.SBPlayPositionLook,
			mcproto.PositionLook(a.X, a.Y, a.Z, a.Yaw, a.Pitch, true))
		s.movedThisTick = true
		s.idleTicks = 0

	case rawevent.KindSprintToggle:
		on, err := rawevent.DecodeToggle(e.Data)
		if err != nil {
			handled = false
			break
		}
		act := mcproto.ActionStopSprint
		if on {
			act = mcproto.ActionStartSprint
		}
		_ = s.send(mcproto.SBPlayEntityAction, mcproto.EntityAction(act))

	case rawevent.KindSneakToggle:
		on, err := rawevent.DecodeToggle(e.Data)
		if err != nil {
			handled = false
			break
		}
		act := mcproto.ActionStopSneak
		if on {
			act = mcproto.ActionStartSneak
		}
		_ = s.send(mcproto.SBPlayEntityAction, mcproto.EntityAction(act))

	case rawevent.KindDig:
		d, err := rawevent.DecodeDig(e.Data)
		if err != nil {
			handled = false
			break
		}
		// A dig is a sequence, not a single packet: START, then however many ticks
		// of destroy progress the block's hardness demands, then FINISH. Capture
		// takes all of it from the wire now, so the trace carries the player's
		// real start, their real pauses, and the digs they began and abandoned.
		//
		// Traces recorded before that still exist, and they hold a lone FINISH —
		// BlockBreakEvent fires once the block is already gone, so it was all
		// there was to record. Replaying a bare FINISH breaks nothing: the vanilla
		// server accepts STOP_DESTROY_BLOCK only for the position it previously
		// saw a START_DESTROY_BLOCK for, and treats any other as client desync,
		// re-sending the block state and dropping the action. So an unpaired
		// FINISH still gets a synthetic START — but one that is now only emitted
		// when the trace did not supply a real one, rather than always.
		key := [3]int32{d.X, d.Y, d.Z}
		if s.digStarted == nil {
			s.digStarted = make(map[[3]int32]bool)
		}
		switch d.Action {
		case mcproto.DigStart:
			s.digStarted[key] = true
		case mcproto.DigFinish:
			if !s.digStarted[key] {
				_ = s.send(mcproto.SBPlayBlockDig,
					mcproto.BlockDig(mcproto.DigStart, d.X, d.Y, d.Z, d.Face, s.nextSeq()))
				_ = s.send(mcproto.SBPlayArmAnimation, mcproto.ArmAnimation(0))
				s.agg.DigStartsSynthesised.Add(1)
			}
			delete(s.digStarted, key)
		case mcproto.DigAbort:
			delete(s.digStarted, key)
		}
		_ = s.send(mcproto.SBPlayBlockDig,
			mcproto.BlockDig(d.Action, d.X, d.Y, d.Z, d.Face, s.nextSeq()))
		_ = s.send(mcproto.SBPlayArmAnimation, mcproto.ArmAnimation(0))
		if d.Action == mcproto.DigFinish {
			s.noteDig(d.X, d.Y, d.Z)
		}

	case rawevent.KindPlaceBlock:
		p, err := rawevent.DecodePlace(e.Data)
		if err != nil {
			handled = false
			break
		}
		// p.X/Y/Z is the block clicked against and p.Face which side of it, which
		// is what use_item_on carries; the server derives the placed position
		// from the two. See CaptureListener.onPlace.
		_ = s.send(mcproto.SBPlayBlockPlace,
			mcproto.BlockPlace(p.Hand, p.X, p.Y, p.Z, p.Face, s.nextSeq()))
		// A real client swings when it places.
		_ = s.send(mcproto.SBPlayArmAnimation, mcproto.ArmAnimation(0))
		s.notePlace(p.X, p.Y, p.Z, p.Face)

	case rawevent.KindUseItem:
		u, err := rawevent.DecodeUseItem(e.Data)
		if err != nil {
			handled = false
			break
		}
		yaw, pitch := s.look()
		_ = s.send(mcproto.SBPlayUseItem,
			mcproto.UseItem(u.Hand, s.nextSeq(), yaw, pitch))

	case rawevent.KindAttackEntity, rawevent.KindInteractEntity:
		// Aim at something that actually exists.
		//
		// Entity ids are assigned per server run, so the captured one is
		// meaningless here — which is why this used to send a bare arm swing and
		// stop. A swing is an animation: no damage, no aggro, no death, no drops,
		// no XP, none of the work that makes combat expensive. The client is told
		// what is nearby though (add_entity), so replay picks a live entity of the
		// captured kind within reach and hits that instead.
		hint, err := rawevent.DecodeEntityRef(e.Data)
		if err != nil {
			handled = false
			break
		}
		s.viewMu.Lock()
		x, y, z := s.view.X, s.view.Y, s.view.Z
		s.viewMu.Unlock()
		wantType, known := mcproto.EntityTypeID[hint.TypeKey]
		if !known {
			wantType = -1
		}
		id, exact, found := s.entities.nearest(x, y+1.62, z, wantType) // eye height
		if !found {
			// Nothing in range. The swing still goes out, because the real client
			// swings whether or not it connects, but the run must not count this
			// as combat reproduced.
			_ = s.send(mcproto.SBPlayArmAnimation, mcproto.ArmAnimation(0))
			s.agg.AttacksNoTarget.Add(1)
			break
		}
		if e.Kind == rawevent.KindAttackEntity {
			_ = s.send(mcproto.SBPlayAttack, mcproto.Attack(id))
			_ = s.send(mcproto.SBPlayArmAnimation, mcproto.ArmAnimation(0))
		} else {
			_ = s.send(mcproto.SBPlayInteract, mcproto.InteractAt(id, hint.Hand, false))
		}
		if exact {
			s.agg.AttacksOnType.Add(1)
		} else {
			s.agg.AttacksOffType.Add(1)
		}

	case rawevent.KindHeldSlot:
		slot, err := rawevent.DecodeHeldSlot(e.Data)
		if err != nil || slot < 0 || slot > 8 {
			handled = false
			break
		}
		// Which tool is in hand decides how long a block takes to break —
		// barehanded stone is 7.5 seconds against a diamond pickaxe's 0.4 — so a
		// bot that never switches reproduces neither the timing of a mining trace
		// nor, on harder blocks, the break at all.
		_ = s.send(mcproto.SBPlaySetCarriedItem, mcproto.SetCarriedItem(slot))

	case rawevent.KindChat:
		msg, err := rawevent.DecodeChat(e.Data)
		if err != nil || msg == "" {
			handled = false
			break
		}
		// Chat is one of the few player actions whose server cost scales with the
		// population rather than the sender: one message is formatted and fanned
		// out to everyone who can see it.
		_ = s.send(mcproto.SBPlayChat,
			mcproto.Chat(expandCommand(msg, s.Username), s.chatClock(), 0))

	case rawevent.KindDropItem:
		full, err := rawevent.DecodeDropItem(e.Data)
		if err != nil {
			handled = false
			break
		}
		// A dropped item becomes an entity that ticks, gets picked up or despawns.
		status := mcproto.DropItem
		if full {
			status = mcproto.DropItemStack
		}
		_ = s.send(mcproto.SBPlayBlockDig, mcproto.BlockDig(status, 0, 0, 0, 0, 0))

	case rawevent.KindSwapHands:
		_ = s.send(mcproto.SBPlayBlockDig, mcproto.BlockDig(mcproto.SwapHands, 0, 0, 0, 0, 0))

	case rawevent.KindUseItemRelease:
		// Finishing a held use — the bow/crossbow shot, the last bite, lowering a
		// shield. The draw went out earlier as a use_item; this is the release,
		// and for a bow it is the packet that actually spawns the arrow.
		_ = s.send(mcproto.SBPlayBlockDig, mcproto.BlockDig(mcproto.ReleaseUseItem, 0, 0, 0, 0, 0))

	case rawevent.KindSwing:
		// The most frequent thing a player sends, and the whole reason to capture
		// it separately: most swings ride along with no other event. Forward the
		// hand the client actually swung. The server re-broadcasts each swing to
		// everyone nearby, so this is animation-fanout load, not a no-op.
		hand, err := rawevent.DecodeSwing(e.Data)
		if err != nil {
			handled = false
			break
		}
		_ = s.send(mcproto.SBPlayArmAnimation, mcproto.ArmAnimation(hand))

	case rawevent.KindCmd:
		raw, err := rawevent.DecodeCmd(e.Data)
		if err != nil {
			handled = false
			break
		}
		if cmd := expandCommand(raw, s.Username); cmd != "" {
			_ = s.send(mcproto.SBPlayChatCommand, mcproto.ChatCommand(cmd))
		}

	case rawevent.KindInvClick:
		// Reproduce the click against whatever window is currently open. The
		// window id and state id are the live values the server assigned (from
		// open_screen / set_content), never the captured ones. Slot/button/mode
		// come from the capture. See docs/PROTOCOL.md.
		c, err := rawevent.DecodeInvClick(e.Data)
		if err != nil {
			handled = false
			break
		}
		_ = s.send(mcproto.SBPlayContainerClick,
			mcproto.ContainerClick(s.curWindow.Load(), s.curState.Load(), c.Slot, c.Button, c.ClickType))

	case rawevent.KindInvClose:
		if win := s.curWindow.Load(); win != 0 {
			_ = s.send(mcproto.SBPlayContainerClose, mcproto.ContainerClose(win))
			s.curWindow.Store(0)
		}

	case rawevent.KindInvOpen:
		// Opening a block container is driven by "use item on" at its position;
		// the server then replies with open_screen carrying the live window id.
		// Only reproducible when the capture recorded a block position (schema
		// with position); player-inventory opens (window 0) need no trigger.
		o, err := rawevent.DecodeInvOpen(e.Data)
		if err != nil || !o.HasPos {
			handled = false
			break
		}
		_ = s.send(mcproto.SBPlayBlockPlace,
			mcproto.BlockPlace(0, o.X, o.Y, o.Z, 1, s.nextSeq()))

	case rawevent.KindCreativeSet:
		cs, err := rawevent.DecodeCreativeSet(e.Data)
		if err != nil {
			handled = false
			break
		}
		_ = s.send(mcproto.SBPlaySetCreativeSlot,
			mcproto.SetCreativeSlot(cs.Slot, cs.ItemID, cs.Count))

	case rawevent.KindMarker, rawevent.KindMobSpawn, rawevent.KindMobDespawn:
		// Annotation / mob-context events with no serverbound analogue.
		handled = false

	default:
		handled = false
	}

	if handled {
		atomic.AddInt64(&s.eventsReplayed, 1)
		s.agg.EventsReplayed.Add(1)
	} else {
		s.agg.EventsSkipped.Add(1)
	}
}

// expandCommand prepares a captured command for the serverbound chat_command
// packet: it expands the {SELF} token to the session username (so a trace can
// address whichever player it is assigned to — e.g. "/eco give {SELF} 100000"),
// then strips the leading slash the packet omits. Returns "" if nothing remains.
func expandCommand(cmd, username string) string {
	if strings.Contains(cmd, "{SELF}") {
		cmd = strings.ReplaceAll(cmd, "{SELF}", username)
	}
	if len(cmd) > 0 && cmd[0] == '/' {
		cmd = cmd[1:]
	}
	return cmd
}

// ---- small helpers used across the flow ----

func mcwireVarInt(b []byte) (int32, error) {
	return mcwire.NewReader(b).VarInt()
}

// decodeText extracts a best-effort human string from a disconnect packet.
// Disconnect reasons are NBT/JSON text components; we surface the raw tail as a
// string rather than fully parsing the component tree.
func decodeText(b []byte) string {
	// Try a leading String (some servers send a length-prefixed JSON string).
	if s, err := mcwire.NewReader(b).String(); err == nil && isPrintable(s) {
		return s
	}
	// Fall back to printable bytes.
	out := make([]byte, 0, len(b))
	for _, c := range b {
		if c >= 0x20 && c < 0x7F {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return "(unreadable reason)"
	}
	return string(out)
}

func isPrintable(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < 0x20 && r != '\n' && r != '\t' {
			return false
		}
	}
	return true
}

// replyPluginRequest answers a login plugin request with "not understood".
func (s *Session) replyPluginRequest(body []byte) error {
	msgID, err := mcwire.NewReader(body).VarInt()
	if err != nil {
		return err
	}
	w := mcwire.NewWriter()
	w.VarInt(msgID)
	w.Bool(false) // not understood, no payload
	return s.send(mcproto.SBLoginPluginResponse, w.Bytes())
}
