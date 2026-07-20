// gen-fixture writes a synthetic capture log (raw-*.bin) so the trace compiler
// and replay client can be exercised without a live Paper server.
//
// Usage:
//
//	gen-fixture --output <capture-logs dir> [--players 3] [--minutes 12] [--seed 1]
package main

import (
	"crypto/sha256"
	"flag"
	"log"
	"math"
	"os"
	"path/filepath"

	"mcbench/internal/rawevent"
	"mcbench/internal/rawlog"
)

func main() {
	output := flag.String("output", "", "capture-logs output directory (required)")
	players := flag.Int("players", 3, "number of synthetic player sessions")
	minutes := flag.Int("minutes", 12, "session length in minutes")
	seed := flag.Uint64("seed", 1, "PRNG seed")
	flag.Parse()
	if *output == "" {
		flag.Usage()
		os.Exit(2)
	}
	if err := os.MkdirAll(*output, 0o755); err != nil {
		log.Fatal(err)
	}

	st := *seed
	next := func() uint64 {
		st += 0x9e3779b97f4a7c15
		z := st
		z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
		z = (z ^ (z >> 27)) * 0x94d049bb133111eb
		return z ^ (z >> 31)
	}
	frand := func() float32 { return float32(next()%2000)/1000 - 1 } // [-1,1)

	durUs := int64(*minutes) * 60 * 1_000_000
	const stepUs = int64(100_000) // 10 events/sec baseline

	var events []rawevent.RawEvent
	for p := 0; p < *players; p++ {
		pid := sha256.Sum256([]byte{byte(p), byte(*seed)})
		yaw := float32(0)
		// Marker at session start.
		events = append(events, rawevent.RawEvent{
			TMicro: 0, PlayerID: pid, SessionSeq: 0, RegionID: "arena",
			Kind: rawevent.KindMarker, Payload: rawevent.EncodeCmd("arena_start"),
		})
		seq := int32(0)
		for t := int64(0); t < durUs; t += stepUs {
			roll := next() % 100
			jitterX := int32((next() % 8))
			ev := rawevent.RawEvent{
				TMicro: t, PlayerID: pid, SessionSeq: 0, DimensionID: 0,
				CoarseChunkX: jitterX / 4, CoarseChunkZ: 0, RegionID: "arena",
			}
			switch {
			case roll < 70: // move
				yaw += frand() * 5
				ev.Kind = rawevent.KindMove
				ev.Payload = rawevent.MovePayload{
					DX: frand() * 0.2, DY: 0, DZ: frand() * 0.2,
					Yaw: yaw, Pitch: frand() * 10, OnGround: true,
				}.Encode()
			case roll < 78: // sprint toggle
				ev.Kind = rawevent.KindSprintToggle
				ev.Payload = rawevent.EncodeToggle(roll%2 == 0)
			case roll < 86: // attack
				ev.Kind = rawevent.KindAttackEntity
				ev.Payload = []byte{byte(next() % 4)} // target_hint varint (small)
			case roll < 92: // dig
				ev.Kind = rawevent.KindDig
				ev.Payload = rawevent.DigPayload{
					Action: 2, X: 10 + jitterX, Y: 64, Z: -5, Face: 1,
				}.Encode()
			case roll < 97: // place
				ev.Kind = rawevent.KindPlaceBlock
				ev.Payload = rawevent.PlacePayload{X: 11, Y: 64, Z: -5, Face: 1, Hand: 0}.Encode()
			default: // command
				ev.Kind = rawevent.KindCmd
				ev.Payload = rawevent.EncodeCmd("/say tick")
			}
			events = append(events, ev)
			seq++
		}
		_ = seq
	}

	// Chunk events into ~5-minute files to mimic rotation.
	perFile := int64(5) * 60 * 1_000_000
	files := int(math.Ceil(float64(durUs) / float64(perFile)))
	if files < 1 {
		files = 1
	}
	written := 0
	for f := 0; f < files; f++ {
		lo := int64(f) * perFile
		hi := lo + perFile
		var chunk []rawevent.RawEvent
		for _, e := range events {
			if e.TMicro >= lo && e.TMicro < hi {
				chunk = append(chunk, e)
			}
		}
		if len(chunk) == 0 {
			continue
		}
		name := filepath.Join(*output, fixtureName(f))
		if err := rawlog.WriteFile(name, "fixture-server", lo/1000, hi/1000, chunk); err != nil {
			log.Fatal(err)
		}
		written += len(chunk)
	}
	log.Printf("wrote %d events across %d players to %s", written, *players, *output)
}

func fixtureName(i int) string {
	return "raw-20260101-000" + string(rune('0'+i)) + ".bin"
}
