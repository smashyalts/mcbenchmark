// dump-capture reads raw-*.bin capture logs (produced by the BenchCapture
// plugin) and prints each event with a decoded payload. Used to confirm that
// what the capture plugin recorded matches what the replay client sent.
//
// Usage: dump-capture <raw-*.bin | capture-logs dir> [--kinds move,cmd,...]
package main

import (
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"

	"mcbench/internal/rawevent"
	"mcbench/internal/rawlog"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: dump-capture <raw-*.bin | dir> [--no-move]")
		os.Exit(2)
	}
	path := os.Args[1]
	noMove := false
	for _, a := range os.Args[2:] {
		if a == "--no-move" {
			noMove = true
		}
	}

	var events []rawevent.RawEvent
	var err error
	if fi, e := os.Stat(path); e == nil && fi.IsDir() {
		events, err = rawlog.ReadDir(path)
	} else {
		events, err = rawlog.ReadFile(path)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "read:", err)
		os.Exit(1)
	}

	// Group by (player, session) to mirror sessionization.
	type key struct {
		pid [32]byte
		seq int32
	}
	order := []key{}
	byKey := map[key][]rawevent.RawEvent{}
	for _, e := range events {
		k := key{e.PlayerID, e.SessionSeq}
		if _, ok := byKey[k]; !ok {
			order = append(order, k)
		}
		byKey[k] = append(byKey[k], e)
	}
	sort.SliceStable(order, func(i, j int) bool {
		return byKey[order[i]][0].TMicro < byKey[order[j]][0].TMicro
	})

	total, shown := 0, 0
	for si, k := range order {
		evs := byKey[k]
		counts := map[int32]int{}
		for _, e := range evs {
			counts[e.Kind]++
		}
		fmt.Printf("\n=== session %d  player=%s..  seq=%d  (%d events) ===\n",
			si, hex.EncodeToString(k.pid[:4]), k.seq, len(evs))
		fmt.Printf("    kinds:")
		var kk []int32
		for kind := range counts {
			kk = append(kk, kind)
		}
		sort.Slice(kk, func(a, b int) bool { return kk[a] < kk[b] })
		for _, kind := range kk {
			fmt.Printf(" %s=%d", rawevent.KindName(kind), counts[kind])
		}
		fmt.Println()
		base := evs[0].TMicro
		for _, e := range evs {
			total++
			if noMove && e.Kind == rawevent.KindMove {
				continue
			}
			shown++
			fmt.Printf("  +%6.2fs %-14s %s\n",
				float64(e.TMicro-base)/1e6, rawevent.KindName(e.Kind), describe(e))
		}
	}
	fmt.Printf("\n%d events total across %d sessions (%d shown)\n", total, len(order), shown)
}

func describe(e rawevent.RawEvent) string {
	switch e.Kind {
	case rawevent.KindCmd:
		s, _ := rawevent.DecodeCmd(e.Payload)
		return fmt.Sprintf("%q", s)
	case rawevent.KindInvOpen:
		o, _ := rawevent.DecodeInvOpen(e.Payload)
		if o.HasPos {
			return fmt.Sprintf("container_type=%d pos=(%d,%d,%d)", o.ContainerType, o.X, o.Y, o.Z)
		}
		return fmt.Sprintf("container_type=%d (no pos)", o.ContainerType)
	case rawevent.KindInvClick:
		c, _ := rawevent.DecodeInvClick(e.Payload)
		return fmt.Sprintf("slot=%d button=%d clickType=%d", c.Slot, c.Button, c.ClickType)
	case rawevent.KindInvClose:
		return ""
	case rawevent.KindMove:
		m, _ := rawevent.DecodeMove(e.Payload)
		return fmt.Sprintf("d=(%.2f,%.2f,%.2f) yaw=%.1f pitch=%.1f ground=%v", m.DX, m.DY, m.DZ, m.Yaw, m.Pitch, m.OnGround)
	case rawevent.KindDig:
		d, _ := rawevent.DecodeDig(e.Payload)
		return fmt.Sprintf("action=%d (%d,%d,%d) face=%d", d.Action, d.X, d.Y, d.Z, d.Face)
	case rawevent.KindPlaceBlock:
		d, _ := rawevent.DecodePlace(e.Payload)
		return fmt.Sprintf("(%d,%d,%d) face=%d hand=%d", d.X, d.Y, d.Z, d.Face, d.Hand)
	case rawevent.KindReanchor:
		a, _ := rawevent.DecodeReanchor(e.Payload)
		return fmt.Sprintf("-> (%.2f,%.2f,%.2f) yaw=%.1f pitch=%.1f dim=%d",
			a.X, a.Y, a.Z, a.Yaw, a.Pitch, a.Dimension)
	case rawevent.KindMarker:
		m, err := rawevent.DecodeMarkerAt(e.Payload)
		if err != nil {
			return "(undecodable marker)"
		}
		if m.HasPos {
			return fmt.Sprintf("%q at (%.2f,%.2f,%.2f) yaw=%.1f pitch=%.1f",
				m.Marker, m.X, m.Y, m.Z, m.Yaw, m.Pitch)
		}
		return fmt.Sprintf("%q", m.Marker)
	case rawevent.KindAttackEntity:
		return "swing/attack"
	default:
		return strings.TrimSpace(fmt.Sprintf("payload=%s", hex.EncodeToString(e.Payload)))
	}
}
