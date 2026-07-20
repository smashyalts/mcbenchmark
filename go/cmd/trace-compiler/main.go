// trace-compiler converts RawEvent capture logs into per-session trace files.
//
// Usage:
//
//	trace-compiler --input <capture-logs dir> --output <trace dir> \
//	    --protocol 775 --world-profile bench-arena-v1 \
//	    --min-duration 600 --max-duration 3600 [--drop-chat] [--run-id id]
package main

import (
	"bufio"
	"encoding/binary"
	"flag"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"

	"mcbench/internal/mcwire"
	"mcbench/internal/rawevent"
	"mcbench/internal/rawlog"
	"mcbench/internal/tracefile"
)

// sessionKey identifies one player's one visit.
type sessionKey struct {
	player [32]byte
	seq    int32
}

// bucketOf spreads sessions across bucket files. Every event of a session must
// land in the same bucket, so pass 2 can group and sort it without holding the
// rest of the capture.
func bucketOf(player [32]byte, seq int32) uint64 {
	h := fnv.New64a()
	h.Write(player[:])
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], uint32(seq))
	h.Write(b[:])
	return h.Sum64()
}

// loadBucket reads one spilled bucket back and groups it by session. Only this
// bucket is resident, which is the whole point of the two passes.
func loadBucket(path string) (map[sessionKey][]rawevent.RawEvent, []sessionKey, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 256<<10)
	sessions := make(map[sessionKey][]rawevent.RawEvent)
	var order []sessionKey
	buf := make([]byte, 0, 512)
	for {
		n, err := binary.ReadUvarint(r)
		if err == io.EOF {
			return sessions, order, nil
		}
		if err != nil {
			return nil, nil, err
		}
		if cap(buf) < int(n) {
			buf = make([]byte, n)
		}
		b := buf[:n]
		if _, err := io.ReadFull(r, b); err != nil {
			return nil, nil, err
		}
		ev, err := rawevent.Decode(mcwire.NewReader(b))
		if err != nil {
			return nil, nil, err
		}
		k := sessionKey{player: ev.PlayerID, seq: ev.SessionSeq}
		if _, ok := sessions[k]; !ok {
			order = append(order, k)
		}
		sessions[k] = append(sessions[k], ev)
	}
}

func main() {
	input := flag.String("input", "", "directory containing raw-*.bin capture logs (required)")
	output := flag.String("output", "", "output directory for compiled traces (required)")
	protocol := flag.Int("protocol", 775, "Minecraft protocol version of the benchmark server")
	worldProfile := flag.String("world-profile", "default", "world/map profile identifier")
	minDuration := flag.Int("min-duration", 600, "minimum session length in seconds (shorter sessions are dropped)")
	maxDuration := flag.Int("max-duration", 3600, "maximum session length in seconds (longer sessions are truncated)")
	dropChat := flag.Bool("drop-chat", false, "drop command payloads (EVENT_CMD) from traces")
	runID := flag.String("run-id", "unnamed", "identifier stored in the manifest")
	bucketCount := flag.Int("buckets", 256, "scratch buckets; peak memory is roughly one bucket, so raise this for very large captures")
	workDir := flag.String("work-dir", "", "directory for scratch bucket files (default: system temp)")
	flag.Parse()

	if *input == "" || *output == "" {
		flag.Usage()
		os.Exit(2)
	}

	buckets := *bucketCount
	if buckets < 1 {
		buckets = 1
	}
	work, err := os.MkdirTemp(*workDir, "trace-compiler-")
	if err != nil {
		log.Fatalf("create work dir: %v", err)
	}
	defer os.RemoveAll(work)

	// Pass 1: stream every capture file and spill each event into one of N
	// bucket files, keyed by a hash of (player_id, session_seq).
	//
	// The previous version read every event into one slice and then duplicated
	// them into per-session slices. Measured at 519 bytes resident per event,
	// which is fine for a fixture and impossible for a real capture: 1500 players
	// for an hour is ~108M events, projecting to ~55 GB. Bucketing bounds peak
	// memory at one bucket instead of the whole capture, at the cost of writing
	// the events to scratch disk once.
	//
	// All events for a session land in the same bucket, which is what makes pass
	// 2 able to group and sort without ever seeing the other buckets.
	bw := make([]*bufio.Writer, buckets)
	bf := make([]*os.File, buckets)
	for i := 0; i < buckets; i++ {
		f, err := os.Create(filepath.Join(work, fmt.Sprintf("b%04d.bin", i)))
		if err != nil {
			log.Fatalf("create bucket: %v", err)
		}
		bf[i] = f
		bw[i] = bufio.NewWriterSize(f, 256<<10)
	}
	total := 0
	enc := mcwire.NewWriter()
	err = rawlog.StreamDir(*input, func(e rawevent.RawEvent) error {
		b := int(bucketOf(e.PlayerID, e.SessionSeq) % uint64(buckets))
		enc.Reset()
		e.Encode(enc)
		var lb [binary.MaxVarintLen64]byte
		n := binary.PutUvarint(lb[:], uint64(enc.Len()))
		if _, err := bw[b].Write(lb[:n]); err != nil {
			return err
		}
		if _, err := bw[b].Write(enc.Bytes()); err != nil {
			return err
		}
		total++
		return nil
	})
	if err != nil {
		log.Fatalf("read capture logs: %v", err)
	}
	for i := 0; i < buckets; i++ {
		if err := bw[i].Flush(); err != nil {
			log.Fatalf("flush bucket: %v", err)
		}
		bf[i].Close()
	}
	log.Printf("read %d raw events from %s into %d buckets", total, *input, buckets)

	if err := os.MkdirAll(*output, 0o755); err != nil {
		log.Fatalf("create output dir: %v", err)
	}

	manifest := &tracefile.Manifest{
		SchemaVersion:   tracefile.SchemaVersion,
		ProtocolVersion: *protocol,
		WorldProfile:    *worldProfile,
		RunID:           *runID,
	}

	minUs := int64(*minDuration) * 1_000_000
	maxUs := int64(*maxDuration) * 1_000_000
	traceNum := 0
	dropped := 0
	sessionCount := 0

	// Pass 2: one bucket at a time, so only that bucket's sessions are resident.
	for i := 0; i < buckets; i++ {
		sessions, order, err := loadBucket(filepath.Join(work, fmt.Sprintf("b%04d.bin", i)))
		if err != nil {
			log.Fatalf("bucket %d: %v", i, err)
		}
		// Deterministic order within the bucket: earliest session first.
		sort.SliceStable(order, func(a, b int) bool {
			return sessions[order[a]][0].TMicro < sessions[order[b]][0].TMicro
		})
		sessionCount += len(order)
		for _, k := range order {
			evs := sessions[k]
			sort.SliceStable(evs, func(a, b int) bool { return evs[a].TMicro < evs[b].TMicro })
			base := evs[0].TMicro
			dur := evs[len(evs)-1].TMicro - base
			if dur < minUs {
				dropped++
				continue
			}

			var tevs []tracefile.TraceEvent
			counts := map[int32]int{}
			for _, e := range evs {
				off := e.TMicro - base
				if off > maxUs {
					break
				}
				if *dropChat && e.Kind == rawevent.KindCmd {
					continue
				}
				counts[e.Kind]++
				tevs = append(tevs, tracefile.TraceEvent{OffsetUs: off, Kind: e.Kind, Data: e.Payload})
			}
			if len(tevs) == 0 {
				dropped++
				continue
			}
			traceNum++
			durUs := tevs[len(tevs)-1].OffsetUs
			name := fmt.Sprintf("trace-%06d.bin", traceNum)
			t := &tracefile.Trace{
				SchemaVersion:   tracefile.SchemaVersion,
				ProtocolVersion: uint32(*protocol),
				WorldProfileID:  *worldProfile,
				TraceID:         fmt.Sprintf("%s-%06d", *runID, traceNum),
				DurationUs:      durUs,
				Events:          tevs,
			}
			if err := t.Write(filepath.Join(*output, name)); err != nil {
				log.Fatalf("write %s: %v", name, err)
			}
			manifest.Traces = append(manifest.Traces, tracefile.ManifestEntry{
				File:      name,
				DurationS: durUs / 1_000_000,
				Events:    len(tevs),
				Tags:      classify(counts, len(tevs)),
			})
		}
	}
	log.Printf("found %d sessions", sessionCount)

	if len(manifest.Traces) == 0 {
		log.Fatalf("no sessions passed the filters (min-duration=%ds); nothing written", *minDuration)
	}
	if err := manifest.Save(*output); err != nil {
		log.Fatalf("write manifest: %v", err)
	}
	log.Printf("wrote %d traces (+%d sessions dropped by filters) and manifest.json to %s",
		len(manifest.Traces), dropped, *output)
}

// classify derives coarse tags from the event mix, for scenario selection.
func classify(counts map[int32]int, total int) []string {
	var tags []string
	combat := counts[rawevent.KindAttackEntity] + counts[rawevent.KindInteractEntity]
	build := counts[rawevent.KindDig] + counts[rawevent.KindPlaceBlock]
	moves := counts[rawevent.KindMove]
	if combat*50 >= total {
		tags = append(tags, "combat")
	}
	if build*20 >= total {
		tags = append(tags, "build")
	}
	if moves*10 >= total*9 {
		tags = append(tags, "traverse")
	}
	if counts[rawevent.KindCmd] > 0 {
		tags = append(tags, "commands")
	}
	if len(tags) == 0 {
		tags = append(tags, "mixed")
	}
	return tags
}
