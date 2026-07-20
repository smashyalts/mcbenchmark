// trace-amplify synthesizes a large, varied trace set from a small real
// capture — e.g. record 5 players, replay 1500.
//
// Each output trace is a copy of a source trace with a per-trace start delay,
// per-event timing jitter, a block offset applied to absolute coordinates, and
// (optionally) varied integer literals in commands, so the clones desync in
// time, spread out in space, and produce distinct values instead of identical
// rows. Usernames need no rewriting because traces use the {SELF} token.
//
// Usage:
//
//	trace-amplify --in <src manifest.json> --out <dir> --count 1500 \
//	    [--seed 1] [--start-jitter-s 60] [--event-jitter-ms 250] \
//	    [--space-spread 512] [--vary-numbers-pct 25]
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"mcbench/internal/amplify"
	"mcbench/internal/tracefile"
)

func main() {
	in := flag.String("in", "", "source manifest.json produced by trace-compiler (required)")
	out := flag.String("out", "", "output directory for amplified traces (required)")
	count := flag.Int("count", 100, "number of traces to synthesize")
	seed := flag.Uint64("seed", 1, "PRNG seed (same seed + inputs = identical output)")
	startJitterS := flag.Int("start-jitter-s", 60, "max random start delay per trace, seconds")
	eventJitterMs := flag.Int("event-jitter-ms", 250, "max +/- per-event timing jitter, milliseconds")
	spaceSpread := flag.Int("space-spread", 512, "max +/- block offset applied to absolute coordinates")
	varyPct := flag.Int("vary-numbers-pct", 25, "vary integer literals in commands by +/- percent (0 = off)")
	runID := flag.String("run-id", "amplified", "run identifier stored in the output manifest")
	flag.Parse()

	if *in == "" || *out == "" || *count < 1 {
		flag.Usage()
		os.Exit(2)
	}

	srcMan, err := tracefile.LoadManifest(*in)
	if err != nil {
		log.Fatalf("load source manifest: %v", err)
	}
	base := filepath.Dir(*in)
	var sources []*tracefile.Trace
	for _, entry := range srcMan.Traces {
		t, err := tracefile.Read(filepath.Join(base, entry.File))
		if err != nil {
			log.Fatalf("read %s: %v", entry.File, err)
		}
		sources = append(sources, t)
	}
	log.Printf("loaded %d source traces from %s", len(sources), *in)

	if err := os.MkdirAll(*out, 0o755); err != nil {
		log.Fatalf("create output dir: %v", err)
	}

	opt := amplify.Options{
		StartJitterUs: int64(*startJitterS) * 1_000_000,
		EventJitterUs: int64(*eventJitterMs) * 1_000,
		SpaceSpread:   int32(*spaceSpread),
		VaryPercent:   *varyPct,
	}
	rng := amplify.NewRNG(*seed)

	outMan := &tracefile.Manifest{
		SchemaVersion:   tracefile.SchemaVersion,
		ProtocolVersion: srcMan.ProtocolVersion,
		WorldProfile:    srcMan.WorldProfile,
		RunID:           *runID,
	}

	for i := 0; i < *count; i++ {
		src := sources[i%len(sources)]
		id := fmt.Sprintf("%s-%06d", *runID, i+1)
		t := amplify.Trace(src, id, opt, rng)
		name := fmt.Sprintf("trace-%06d.bin", i+1)
		if err := t.Write(filepath.Join(*out, name)); err != nil {
			log.Fatalf("write %s: %v", name, err)
		}
		// Carry the source's tags through so scenario selection still works.
		tags := srcMan.Traces[i%len(srcMan.Traces)].Tags
		outMan.Traces = append(outMan.Traces, tracefile.ManifestEntry{
			File: name, DurationS: t.DurationUs / 1_000_000, Events: len(t.Events), Tags: tags,
		})
	}
	if err := outMan.Save(*out); err != nil {
		log.Fatalf("write manifest: %v", err)
	}
	log.Printf("amplified %d source traces -> %d traces in %s (seed=%d, start-jitter=%ds, spread=%d blocks, vary=%d%%)",
		len(sources), *count, *out, *seed, *startJitterS, *spaceSpread, *varyPct)
}
