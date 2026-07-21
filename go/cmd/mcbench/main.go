// mcbench turns a capture into a running benchmark in one command.
//
//	mcbench run <capture.bin | capture-dir> --world <server>/world
//
// That is the whole happy path. It compiles the capture, places the bench
// accounts, waits for the server, replays, and writes the report.
//
// The four-step version of this — trace-compiler, then a hand-written
// scenario.yaml, then bench-playerdata with flags that have to agree with the
// yaml, then mc-replay — was not merely tedious. Every one of those seams had a
// way to fail quietly: a --count that did not match target_players, a --prefix
// that did not match username_prefix, a manifest path pointing at last week's
// traces. The failures all looked the same from the outside, which is a run that
// connects and changes nothing.
//
// The individual tools still exist and still work; this drives them in-process,
// with the values that have to agree derived from one place.
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mcbench/internal/compile"
	"mcbench/internal/playerdata"
	"mcbench/internal/replay"
	"mcbench/internal/scenario"
	"mcbench/internal/tracefile"

	"gopkg.in/yaml.v3"
)

func main() {
	log.SetFlags(log.Ltime)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "run":
		runCmd(os.Args[2:])
	case "compile":
		compileCmd(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "mcbench: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

// splitPositional lifts a leading non-flag argument out of the argument list.
//
// Go's flag package stops parsing at the first non-flag argument, so
// "mcbench run capture.bin --world ..." would silently ignore every flag after
// the path — the run would use default values and say nothing about it. That is
// also the order everyone types, so it has to work rather than be documented
// against.
func splitPositional(args []string) (rest []string, positional string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[1:], args[0]
	}
	return args, ""
}

func usage() {
	fmt.Fprint(os.Stderr, `mcbench — capture in, benchmark out.

  mcbench run <capture.bin | capture-dir> --world <server>/world
      Compile, place the bench accounts, wait for the server, replay.

  mcbench compile <capture.bin | capture-dir> --out <dir>
      Compile only, for inspecting traces or reusing them across runs.

Run "mcbench run --help" for the full flag list.
`)
}

func compileCmd(args []string) {
	args, positional := splitPositional(args)
	fs := flag.NewFlagSet("compile", flag.ExitOnError)
	o := compile.Defaults()
	out := fs.String("out", "", "output directory (default: <input>/traces)")
	fs.IntVar(&o.Protocol, "protocol", o.Protocol, "protocol version of the benchmark server")
	fs.StringVar(&o.WorldProfile, "world-profile", o.WorldProfile, "label recording which world these coordinates belong to")
	fs.IntVar(&o.MinDuration, "min-seconds", 0, "drop sessions shorter than this")
	fs.IntVar(&o.MaxDuration, "max-seconds", o.MaxDuration, "truncate sessions longer than this")
	fs.StringVar(&o.RunID, "run-id", o.RunID, "identifier stored in the manifest")
	_ = fs.Parse(args)
	if positional == "" {
		positional = fs.Arg(0)
	}

	input := positional
	if input == "" {
		fs.Usage()
		os.Exit(2)
	}
	dir, err := captureDir(input)
	if err != nil {
		log.Fatal(err)
	}
	o.Input = dir
	o.Output = *out
	if o.Output == "" {
		o.Output = filepath.Join(dir, "traces")
	}
	if _, err := compile.Run(o); err != nil {
		log.Fatal(err)
	}
}

func runCmd(args []string) {
	args, positional := splitPositional(args)
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	world := fs.String("world", "", "the benchmark server's world directory; bots are placed in it before they log in")
	target := fs.String("target", "127.0.0.1:25565", "benchmark server address")
	players := fs.Int("players", 0, "how many bots (default: one per trace)")
	minutes := fs.Int("minutes", 0, "run for this long (default: one pass of each trace, then stop)")
	prefix := fs.String("prefix", "BENCH_", "bench account username prefix, max 11 characters")
	outDir := fs.String("out", "", "where to write run.json (default: ./runs/<timestamp>)")
	tracesDir := fs.String("traces", "", "reuse an existing compiled traces directory instead of compiling")
	importDir := fs.String("import", "", "production player data directory, to give bots the real players' gear")
	protocol := fs.Int("protocol", 775, "protocol version of the benchmark server")
	minSeconds := fs.Int("min-seconds", 0, "drop captured sessions shorter than this")
	connectPerSec := fs.Int("connect-per-second", 20, "login rate limit")
	skipPlace := fs.Bool("skip-place", false, "do not touch player data (bots spawn wherever the server puts them)")
	waitFor := fs.Duration("wait", 5*time.Minute, "how long to wait for the server to come up")
	writeScenario := fs.String("write-scenario", "", "also write the generated scenario to this path, to edit and reuse")
	fs.Parse(args)
	if positional == "" {
		positional = fs.Arg(0)
	}

	if len(*prefix) > 11 {
		log.Fatalf("--prefix %q is %d characters; the 5-digit account index leaves room for 11",
			*prefix, len(*prefix))
	}

	// 1. Traces: compile the capture, or reuse a directory that already has them.
	manifestPath := ""
	if *tracesDir != "" {
		manifestPath = filepath.Join(*tracesDir, "manifest.json")
	} else {
		input := positional
		if input == "" {
			fmt.Fprintln(os.Stderr, "mcbench run: give a capture file or directory, or --traces <dir>")
			fs.Usage()
			os.Exit(2)
		}
		dir, err := captureDir(input)
		if err != nil {
			log.Fatal(err)
		}
		o := compile.Defaults()
		o.Input = dir
		o.Output = filepath.Join(dir, "traces")
		o.Protocol = *protocol
		o.MinDuration = *minSeconds
		o.RunID = "mcbench"
		// A capture of a few minutes is the normal case for a quick check, and
		// the compiler's hour-long default ceiling has no reason to apply here.
		if _, err := compile.Run(o); err != nil {
			log.Fatal(err)
		}
		manifestPath = filepath.Join(o.Output, "manifest.json")
	}

	man, err := tracefile.LoadManifest(manifestPath)
	if err != nil {
		log.Fatal(err)
	}
	count := *players
	if count <= 0 {
		count = len(man.Traces)
	}

	// 2. Place the accounts. This has to happen with the server stopped, so the
	//    check is a connection attempt rather than a promise in the docs.
	if *skipPlace {
		log.Printf("WARNING: --skip-place, so bots spawn wherever the server puts them. " +
			"If that is not where their trace was captured, block events do nothing.")
	} else if *world == "" {
		log.Printf("WARNING: no --world given, so bench accounts are not placed. They will " +
			"spawn at world spawn, out of range of every block their traces touch. " +
			"Pass --world <server>/world, or --skip-place to silence this.")
	} else {
		if reachable(*target) {
			log.Fatalf("the server at %s is running, and player data written underneath a "+
				"running server is ignored or overwritten. Stop it and run this again — "+
				"mcbench will wait for you to start it back up.", *target)
		}
		po := playerdata.Defaults()
		po.World = *world
		po.Manifest = manifestPath
		po.Prefix = *prefix
		po.Count = count
		po.ImportDir = *importDir
		if err := playerdata.Run(po); err != nil {
			log.Fatal(err)
		}
	}

	// 3. Wait for the server. Placing accounts requires it down and replaying
	//    requires it up, so exactly one pause belongs in this command.
	if !reachable(*target) {
		log.Printf("waiting for %s — start the benchmark server now", *target)
		if !waitReachable(*target, *waitFor) {
			log.Fatalf("%s did not come up within %s", *target, *waitFor)
		}
	}
	log.Printf("%s is up", *target)

	// 4. Replay, from a scenario built here rather than one kept in step with
	//    these flags by hand.
	sc := buildScenario(*target, manifestPath, *prefix, count, *minutes, *protocol, *connectPerSec)
	if *writeScenario != "" {
		if err := saveScenario(sc, *writeScenario); err != nil {
			log.Fatal(err)
		}
		log.Printf("scenario written to %s", *writeScenario)
	}
	dir := *outDir
	if dir == "" {
		dir = filepath.Join("runs", time.Now().Format("20060102-150405"))
	}
	r, err := replay.New(sc, dir)
	if err != nil {
		log.Fatal(err)
	}
	if err := r.Run(); err != nil {
		log.Fatal(err)
	}
	log.Printf("done — report in %s", filepath.Join(dir, "run.json"))
}

// buildScenario derives the whole scenario from the run's flags.
//
// The account prefix and count appear in two places that must agree — the
// player data written to disk and the names the bots log in with — and this is
// the one place they are decided.
func buildScenario(target, manifest, prefix string, count, minutes, protocol, connectPerSec int) *scenario.Scenario {
	host, port := splitTarget(target)
	sc := &scenario.Scenario{Name: "mcbench"}
	sc.Target.Host = host
	sc.Target.Port = port
	sc.Protocol.Version = protocol
	sc.Traces.Manifest = manifest
	sc.Traces.Selection.Strategy = "round_robin"
	sc.Load.TargetPlayers = count
	sc.Load.Ramp.InitialPlayers = count
	sc.Load.Ramp.IntervalSeconds = 5
	sc.Limits.ConnectPerSecond = connectPerSec
	sc.Identity.UsernamePrefix = prefix
	if minutes > 0 {
		// Long run: loop the traces to fill the time.
		sc.Limits.MaxDurationMinutes = minutes
		sc.Traces.PerSessionMinutes = minutes
		sc.Traces.ReusePolicy = "allow_with_jitter"
	} else {
		// Default: play each trace once and stop, which is what someone
		// checking a capture actually wants. The ceiling is a safety net.
		sc.Traces.ReusePolicy = "once"
		sc.Limits.MaxDurationMinutes = 60
	}
	return sc
}

func saveScenario(sc *scenario.Scenario, path string) error {
	b, err := yaml.Marshal(sc)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func splitTarget(target string) (string, int) {
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return target, 25565
	}
	port := 25565
	fmt.Sscanf(portStr, "%d", &port)
	return host, port
}

// captureDir accepts either a directory of raw-*.bin files or a single one, so
// "the file I just copied off the server" works without shuffling it into a
// directory first.
func captureDir(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if fi.IsDir() {
		return path, nil
	}
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	if !strings.HasPrefix(base, "raw-") || !strings.HasSuffix(base, ".bin") {
		return "", fmt.Errorf("%s is not a capture log: the compiler reads raw-*.bin, "+
			"and pointing it at this file's directory would silently read nothing", path)
	}
	// The compiler globs a directory. Naming a single file inside one that holds
	// several would quietly pull in its neighbours, so say what is happening.
	if others, _ := filepath.Glob(filepath.Join(dir, "raw-*.bin")); len(others) > 1 {
		log.Printf("note: %s holds %d capture files; compiling all of them", dir, len(others))
	}
	return dir, nil
}

func reachable(target string) bool {
	c, err := net.DialTimeout("tcp", target, 2*time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func waitReachable(target string, limit time.Duration) bool {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if reachable(target) {
			// A server that accepts TCP is still loading chunks for a moment.
			time.Sleep(2 * time.Second)
			return true
		}
		time.Sleep(time.Second)
	}
	return false
}
