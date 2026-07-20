// mc-replay replays compiled traces against an offline-mode benchmark server.
//
// Usage:
//
//	mc-replay --scenario scenarios/1h-default.yaml [--out-dir runs/<ts>]
package main

import (
	"flag"
	"log"
	"os"
	"path/filepath"

	"mcbench/internal/replay"
	"mcbench/internal/scenario"
)

func main() {
	scenarioPath := flag.String("scenario", "", "path to scenario YAML (required)")
	outDir := flag.String("out-dir", "", "output directory for run report (default: <scenario output.dir>/run)")
	flag.Parse()

	if *scenarioPath == "" {
		flag.Usage()
		os.Exit(2)
	}

	sc, err := scenario.Load(*scenarioPath)
	if err != nil {
		log.Fatalf("load scenario: %v", err)
	}

	dir := *outDir
	if dir == "" {
		base := sc.Output.Dir
		if base == "" {
			base = "runs"
		}
		dir = filepath.Join(base, "run")
	}

	r, err := replay.New(sc, dir)
	if err != nil {
		log.Fatalf("init runner: %v", err)
	}
	if err := r.Run(); err != nil {
		log.Fatalf("run: %v", err)
	}
}
