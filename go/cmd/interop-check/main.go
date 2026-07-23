// interop-check decodes a capture log produced by the Java InteropFixture and
// verifies each field matches the expected values, proving Java<->Go byte
// compatibility. Exits non-zero on any mismatch.
package main

import (
	"crypto/sha256"
	"fmt"
	"os"

	"mcbench/internal/mcproto"
	"mcbench/internal/rawevent"
	"mcbench/internal/rawlog"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: interop-check <raw-*.bin>")
		os.Exit(2)
	}
	events, err := rawlog.ReadFile(os.Args[1])
	if err != nil {
		fail("read: %v", err)
	}
	if len(events) != 20 {
		fail("expected 20 events, got %d", len(events))
	}

	pid := sha256.Sum256([]byte("player-uuid-0|salt"))
	e0 := events[0]
	if e0.PlayerID != pid {
		fail("player id mismatch")
	}
	if e0.RegionID != "arena" || e0.CoarseChunkX != 2 || e0.Kind != rawevent.KindMarker {
		fail("event0 header mismatch: %+v", e0)
	}
	if m, _ := rawevent.DecodeMarker(e0.Payload); m != "arena_start" {
		fail("marker mismatch: %q", m)
	}

	mv, err := rawevent.DecodeMove(events[1].Payload)
	if err != nil {
		fail("move decode: %v", err)
	}
	if mv.DX != 0.1 || mv.DZ != -0.2 || mv.Yaw != 90.5 || mv.Pitch != 12.25 || !mv.OnGround {
		fail("move payload mismatch: %+v", mv)
	}

	if on, _ := rawevent.DecodeToggle(events[2].Payload); !on {
		fail("sprint toggle mismatch")
	}

	dig, _ := rawevent.DecodeDig(events[3].Payload)
	if dig.Action != 2 || dig.X != 10 || dig.Y != 64 || dig.Z != -5 || dig.Face != 1 {
		fail("dig payload mismatch: %+v", dig)
	}

	pl, _ := rawevent.DecodePlace(events[4].Payload)
	if pl.X != 11 || pl.Face != 1 || pl.Hand != 0 {
		fail("place payload mismatch: %+v", pl)
	}

	if cmd, _ := rawevent.DecodeCmd(events[6].Payload); cmd != "/say hello world" {
		fail("cmd payload mismatch: %q", cmd)
	}

	neg := events[7]
	if neg.CoarseChunkX != -3 || neg.CoarseChunkZ != -7 {
		fail("negative varint mismatch: x=%d z=%d", neg.CoarseChunkX, neg.CoarseChunkZ)
	}
	nmv, _ := rawevent.DecodeMove(neg.Payload)
	if nmv.DX != -1.5 || nmv.Yaw != -180 || nmv.Pitch != -45 || nmv.OnGround {
		fail("negative move payload mismatch: %+v", nmv)
	}

	if m, _ := rawevent.DecodeMarker(events[8].Payload); m != "round_end" {
		fail("second frame marker mismatch: %q", m)
	}

	// The entity reference is a registry key now, not an enum ordinal, and it
	// must resolve to a protocol id or replay cannot aim an attack.
	ref, err := rawevent.DecodeEntityRef(events[5].Payload)
	if err != nil || ref.TypeKey != "minecraft:zombie" {
		fail("entity ref mismatch: %+v (%v)", ref, err)
	}
	if _, ok := mcproto.EntityTypeID[ref.TypeKey]; !ok {
		fail("entity key %q has no protocol id", ref.TypeKey)
	}

	// The kinds captured from the wire rather than from Bukkit events.
	if slot, _ := rawevent.DecodeHeldSlot(events[12].Payload); slot != 4 {
		fail("held slot mismatch: %d", slot)
	}
	if msg, _ := rawevent.DecodeChat(events[13].Payload); msg != "selling 64 diamonds at spawn" {
		fail("chat mismatch: %q", msg)
	}
	if full, _ := rawevent.DecodeDropItem(events[14].Payload); !full {
		fail("drop item should be a full stack")
	}
	if events[15].Kind != rawevent.KindSwapHands || len(events[15].Payload) != 0 {
		fail("swap hands mismatch: %+v", events[15])
	}
	if d, _ := rawevent.DecodeDig(events[16].Payload); d.Action != 0 || d.X != 10 {
		fail("dig start mismatch: %+v", d)
	}
	if events[17].Kind != rawevent.KindSwing {
		fail("swing kind mismatch: %+v", events[17])
	}
	if hand, _ := rawevent.DecodeSwing(events[17].Payload); hand != 1 {
		fail("swing hand mismatch: %d", hand)
	}
	if events[18].Kind != rawevent.KindUseItemRelease || len(events[18].Payload) != 0 {
		fail("use-item release mismatch: %+v", events[18])
	}
	if action, boost, _ := rawevent.DecodeEntityAction(events[19].Payload); action != 5 || boost != 100 {
		fail("entity action mismatch: action=%d boost=%d", action, boost)
	}

	fmt.Printf("OK: decoded %d events across frames, all fields match\n", len(events))
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "INTEROP FAIL: "+format+"\n", a...)
	os.Exit(1)
}
