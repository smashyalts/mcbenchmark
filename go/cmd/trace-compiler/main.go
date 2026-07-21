// trace-compiler converts RawEvent capture logs into per-session trace files.
//
// The compile itself lives in internal/compile, so `mcbench run` performs the
// identical work in-process rather than a second implementation of it. This is
// the flag surface and nothing else.
//
//	trace-compiler --input <capture-logs dir> --output <trace dir> \
//	    --protocol 775 --world-profile bench-arena-v1 \
//	    --min-duration 600 --max-duration 3600 [--drop-chat] [--run-id id]
package main

import (
	"flag"
	"log"
	"os"

	"mcbench/internal/compile"
)

func main() {
	o := compile.Defaults()
	flag.StringVar(&o.Input, "input", "", "directory containing raw-*.bin capture logs (required)")
	flag.StringVar(&o.Output, "output", "", "output directory for compiled traces (required)")
	flag.IntVar(&o.Protocol, "protocol", o.Protocol, "Minecraft protocol version of the benchmark server")
	flag.StringVar(&o.WorldProfile, "world-profile", o.WorldProfile, "world/map profile identifier")
	flag.IntVar(&o.MinDuration, "min-duration", o.MinDuration, "minimum session length in seconds (shorter sessions are dropped)")
	flag.IntVar(&o.MaxDuration, "max-duration", o.MaxDuration, "maximum session length in seconds (longer sessions are truncated)")
	flag.BoolVar(&o.DropChat, "drop-chat", false, "drop command payloads (EVENT_CMD) from traces")
	flag.StringVar(&o.RunID, "run-id", o.RunID, "identifier stored in the manifest")
	flag.IntVar(&o.Buckets, "buckets", o.Buckets, "scratch buckets; peak memory is roughly one bucket, so raise this for very large captures")
	flag.StringVar(&o.WorkDir, "work-dir", "", "directory for scratch bucket files (default: system temp)")
	flag.Parse()

	if o.Input == "" || o.Output == "" {
		flag.Usage()
		os.Exit(2)
	}
	if _, err := compile.Run(o); err != nil {
		log.Fatal(err)
	}
}
