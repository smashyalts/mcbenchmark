// gen-demo writes a purpose-built trace set for end-to-end validation against a
// real server. Unlike gen-fixture (random walk near spawn), each trace here:
//   - walks in a straight line for --distance blocks, forcing the server to
//     generate and stream new chunks/regions along the path;
//   - performs digs and player-inventory clicks;
//   - in creative mode, sets inventory slots so the items persist to player NBT.
//
// Usage:
//
//	gen-demo --output <trace dir> [--players 2] [--distance 240] [--creative]
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"mcbench/internal/rawevent"
	"mcbench/internal/tracefile"
)

// A few real 26.1.2 item ids (from the server registry report).
const (
	itemDiamond      = 899
	itemDiamondBlock = 93
	itemGoldenApple  = 987
)

func main() {
	output := flag.String("output", "", "output trace directory (required)")
	players := flag.Int("players", 2, "number of demo traces")
	distance := flag.Int("distance", 240, "straight-line walk distance in blocks")
	protocol := flag.Int("protocol", 775, "protocol version stored in traces/manifest")
	creative := flag.Bool("creative", false, "include creative inventory-set events (server must be creative)")
	tp := flag.Bool("tp", false, "teleport to distant waypoints via /tp (players must be opped) to force chunk generation")
	ah := flag.Bool("ah", false, "NexusAuctionHouse flow: alternating seller/buyer traces (count from --players, min 2)")
	price := flag.Int("price", 100, "auction-house sell price for --ah mode")
	buyerMoney := flag.Int("buyer-money", 100000, "--ah: amount each buyer grants itself via /eco give {SELF} before buying")
	flag.Parse()
	if *output == "" {
		flag.Usage()
		os.Exit(2)
	}
	if err := os.MkdirAll(*output, 0o755); err != nil {
		log.Fatal(err)
	}

	manifest := &tracefile.Manifest{
		SchemaVersion:   tracefile.SchemaVersion,
		ProtocolVersion: *protocol,
		WorldProfile:    "demo-walk",
		RunID:           "demo",
	}

	if *ah {
		writeAHTraces(*output, *protocol, *price, *players, *buyerMoney, manifest)
		if err := manifest.Save(*output); err != nil {
			log.Fatal(err)
		}
		sellers := (*players + 1) / 2
		log.Printf("wrote %d auction-house traces (%d sellers, %d buyers; price=%d, buyer money=%d) to %s",
			len(manifest.Traces), sellers, *players-sellers, *price, *buyerMoney, *output)
		return
	}

	const stepUs = int64(50_000) // one move per server tick (20 Hz)
	for p := 0; p < *players; p++ {
		var evs []tracefile.TraceEvent
		t := int64(0)
		add := func(kind int32, data []byte) {
			evs = append(evs, tracefile.TraceEvent{OffsetUs: t, Kind: kind, Data: data})
		}

		add(rawevent.KindMarker, encMarker("demo_start"))

		// Creative inventory fill first, so items are set before we move.
		// Slots are player-inventory-menu indices: 36-44 = hotbar, 9-35 = main.
		// (0-8 are crafting/armor and are NOT persisted to the Inventory NBT.)
		if *creative {
			add(rawevent.KindCreativeSet, csEnc(36, itemDiamond, 64)) // hotbar slot 1
			t += stepUs
			add(rawevent.KindCreativeSet, csEnc(37, itemDiamondBlock, 16)) // hotbar slot 2
			t += stepUs
			add(rawevent.KindCreativeSet, csEnc(44, itemGoldenApple, 3)) // hotbar slot 9
			t += stepUs
			add(rawevent.KindCreativeSet, csEnc(9, itemDiamond, 32)) // main inventory
			t += stepUs
		}

		if *tp {
			// Teleport mode: op'd players jump to distant waypoints, forcing the
			// server to generate chunks/regions far from spawn. Each waypoint is
			// >512 blocks out so it lands in a brand-new region file.
			waypoints := [][2]int{{1600, 0}, {0, 1600}, {-1600, 0}, {0, -1600}, {1600, 1600}}
			for _, wp := range waypoints {
				x := wp[0] + p*40 // offset per player so they don't share a chunk
				z := wp[1] + p*40
				add(rawevent.KindCmd, rawevent.EncodeCmd(fmt.Sprintf("/tp @s %d 100 %d", x, z)))
				t += 3_000_000 // 3s between teleports so chunks load
			}
		} else {
			// Fly in a per-player direction so players fan out and each opens its
			// own swath of chunks. 0.5 blocks/tick (10 blocks/s) stays under the
			// server's movement-speed check; on_ground=false = creative flight.
			dirs := [][2]float32{{0.5, 0}, {0, 0.5}, {0.35, 0.35}, {-0.35, 0.35}}
			d := dirs[p%len(dirs)]
			steps := *distance * 2 // ~0.5 blocks per step
			yaw := float32(0)
			for i := 0; i < steps; i++ {
				add(rawevent.KindMove, mvEnc(d[0], 0, d[1], yaw, 0, false))
				t += stepUs
				// Occasional player-inventory click (window 0) and a dig.
				if i%40 == 20 {
					add(rawevent.KindInvClick, icEnc(0, int32(36+(i/40)%9), 0, 0))
					t += stepUs
				}
				if i%60 == 30 {
					add(rawevent.KindDig, digEnc(2, int32(i), 63, 0, 1))
					t += stepUs
				}
			}
		}
		add(rawevent.KindMarker, encMarker("demo_end"))

		name := fmt.Sprintf("trace-%06d.bin", p+1)
		tr := &tracefile.Trace{
			SchemaVersion:   tracefile.SchemaVersion,
			ProtocolVersion: uint32(*protocol),
			WorldProfileID:  "demo-walk",
			TraceID:         fmt.Sprintf("demo-%06d", p+1),
			DurationUs:      t,
			Events:          evs,
		}
		if err := tr.Write(filepath.Join(*output, name)); err != nil {
			log.Fatal(err)
		}
		manifest.Traces = append(manifest.Traces, tracefile.ManifestEntry{
			File: name, DurationS: t / 1_000_000, Events: len(evs), Tags: []string{"demo", "traverse"},
		})
	}
	if err := manifest.Save(*output); err != nil {
		log.Fatal(err)
	}
	log.Printf("wrote %d demo traces (distance=%d, creative=%v) to %s",
		*players, *distance, *creative, *output)
}

// ahItemSlots are the MainGui slots that hold listings: everything except the
// left column (0,9,18,27,36,45 — filled by fillSide(LEFT)) and the bottom row
// (45-53 — fillBottom). Buyers spread their clicks across these so a fleet of
// buyers targets different listings instead of all contending for slot 1.
func ahItemSlots() []int32 {
	var s []int32
	for slot := int32(0); slot < 45; slot++ { // rows 1-5; row 6 is nav
		if slot%9 == 0 {
			continue // left column filler
		}
		s = append(s, slot)
	}
	return s
}

// writeAHTraces emits `users` ordered traces exercising NexusAuctionHouse:
// even indices are sellers (creative-set an item, /ah sell <price>, confirm at
// slot 20), odd indices are buyers (/eco give {SELF} <money>, /ah, click a
// spread listing slot, confirm at slot 20). Round-robin assignment maps trace i
// to player i (username prefix+i), and the {SELF} token expands to that runtime
// username so each buyer funds its own account. Confirm-button slot (20) and the
// listing-slot layout were read from the plugin's GUI classes.
func writeAHTraces(dir string, protocol, price, users, buyerMoney int, manifest *tracefile.Manifest) {
	const confirmSlot = 20 // ConfirmSell/ConfirmBuy confirm button (row 3, col 3)
	if users < 2 {
		users = 2
	}
	itemSlots := ahItemSlots()

	write := func(file, id string, evs []tracefile.TraceEvent, tags []string) {
		dur := evs[len(evs)-1].OffsetUs
		tr := &tracefile.Trace{
			SchemaVersion: tracefile.SchemaVersion, ProtocolVersion: uint32(protocol),
			WorldProfileID: "ah-demo", TraceID: id, DurationUs: dur, Events: evs,
		}
		if err := tr.Write(filepath.Join(dir, file)); err != nil {
			log.Fatal(err)
		}
		manifest.Traces = append(manifest.Traces, tracefile.ManifestEntry{
			File: file, DurationS: dur / 1_000_000, Events: len(evs), Tags: tags,
		})
	}

	buyerOrd := 0
	for i := 0; i < users; i++ {
		file := fmt.Sprintf("trace-%06d.bin", i+1)
		if i%2 == 0 {
			// Seller: creative-set an item into hand, list it, confirm.
			seller := []tracefile.TraceEvent{
				{OffsetUs: 0, Kind: rawevent.KindMarker, Data: encMarker("seller_start")},
				{OffsetUs: 300_000, Kind: rawevent.KindCreativeSet, Data: csEnc(36, itemDiamond, 64)},
				{OffsetUs: 2_000_000, Kind: rawevent.KindCmd, Data: rawevent.EncodeCmd(fmt.Sprintf("/ah sell %d", price))},
				{OffsetUs: 3_500_000, Kind: rawevent.KindInvClick, Data: icEnc(0, confirmSlot, 0, 0)},
				{OffsetUs: 4_500_000, Kind: rawevent.KindMarker, Data: encMarker("seller_listed")},
			}
			write(file, fmt.Sprintf("ah-seller-%03d", i), seller, []string{"ah", "seller", "commands", "container"})
		} else {
			// Buyer: fund self via {SELF}, open AH, click a spread listing slot,
			// confirm. Buyers are delayed so sellers have listed first.
			listingSlot := itemSlots[buyerOrd%len(itemSlots)]
			buyerOrd++
			buyer := []tracefile.TraceEvent{
				{OffsetUs: 0, Kind: rawevent.KindMarker, Data: encMarker("buyer_start")},
				{OffsetUs: 300_000, Kind: rawevent.KindCmd, Data: rawevent.EncodeCmd(fmt.Sprintf("/eco give {SELF} %d", buyerMoney))},
				{OffsetUs: 6_000_000, Kind: rawevent.KindCmd, Data: rawevent.EncodeCmd("/ah")},
				{OffsetUs: 7_500_000, Kind: rawevent.KindInvClick, Data: icEnc(0, listingSlot, 0, 0)},
				{OffsetUs: 9_000_000, Kind: rawevent.KindInvClick, Data: icEnc(0, confirmSlot, 0, 0)},
				{OffsetUs: 10_000_000, Kind: rawevent.KindMarker, Data: encMarker("buyer_bought")},
			}
			write(file, fmt.Sprintf("ah-buyer-%03d", i), buyer, []string{"ah", "buyer", "commands", "container"})
		}
	}
}

func encMarker(s string) []byte { return rawevent.EncodeCmd(s) } // marker uses String payload
func mvEnc(dx, dy, dz, yaw, pitch float32, g bool) []byte {
	return rawevent.MovePayload{DX: dx, DY: dy, DZ: dz, Yaw: yaw, Pitch: pitch, OnGround: g}.Encode()
}
func digEnc(a, x, y, z, f int32) []byte {
	return rawevent.DigPayload{Action: a, X: x, Y: y, Z: z, Face: f}.Encode()
}
func icEnc(win, slot, btn, ct int32) []byte {
	return rawevent.InvClickPayload{WindowID: win, Slot: slot, Button: btn, ClickType: ct}.Encode()
}
func csEnc(slot, item, count int32) []byte {
	return rawevent.CreativeSetPayload{Slot: slot, ItemID: item, Count: count}.Encode()
}
