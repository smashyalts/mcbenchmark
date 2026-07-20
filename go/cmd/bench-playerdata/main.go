// bench-playerdata places benchmark accounts in the world before they connect.
//
// A replay bot cannot choose where it spawns: the server decides, and for an
// account that has never logged in that means world spawn. Two things break as
// a result, both silently:
//
//   - Block events do nothing. Dig and place carry absolute coordinates from the
//     capture, and a bot standing at spawn is nowhere near them, so the server
//     rejects every one as out of range. The run reports the events as replayed
//     because they were sent; the world never changes.
//   - Bots get kicked. A player suspended in mid-air with no block beneath is
//     "floating", and the server disconnects it with "Flying is not enabled on
//     this server" after four seconds. World spawn is not reliably solid ground.
//
// The fix is to write each account's player data file with the position its
// trace was captured at, which is what a returning player's data would contain.
//
// Usage:
//
//	bench-playerdata --world server/world --manifest traces/manifest.json \
//	    --prefix BENCH_ --count 500
//	bench-playerdata --world server/world --prefix BENCH_ --count 500 --remove
//
// Run it with the server stopped. Paper reads player data at login and writes it
// back at logout, so a file written underneath a running server is either
// ignored or overwritten.
package main

import (
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"mcbench/internal/mcproto"
	"mcbench/internal/nbt"
	"mcbench/internal/tracefile"
)

// dimensionNames maps the capture plugin's dimension ids (CaptureManager
// .dimensionId) to the resource keys player data stores.
var dimensionNames = map[int32]string{
	0: "minecraft:overworld",
	1: "minecraft:the_nether",
	2: "minecraft:the_end",
}

func main() {
	world := flag.String("world", "", "world directory containing playerdata/ (required)")
	manifest := flag.String("manifest", "", "trace manifest; positions come from the traces it lists")
	prefix := flag.String("prefix", "BENCH_", "username prefix, must match the scenario's identity.username_prefix")
	count := flag.Int("count", 0, "number of accounts, must match the scenario's load.target_players (required)")
	originFlag := flag.String("origin", "", "fallback position \"x,y,z\" for traces that carry none")
	gameMode := flag.Int("gamemode", 0, "playerGameType: 0 survival, 1 creative, 2 adventure, 3 spectator")
	dataVersion := flag.Int("data-version", 0, "override the DataVersion (default: read from the world's level.dat)")
	dirFlag := flag.String("dir", "", "player data directory (default: auto-detected under --world)")
	remove := flag.Bool("remove", false, "delete the generated files instead of writing them")
	dryRun := flag.Bool("dry-run", false, "report what would be written and exit")
	flag.Parse()

	if *world == "" || *count <= 0 {
		flag.Usage()
		os.Exit(2)
	}
	playerDir := *dirFlag
	if playerDir == "" {
		playerDir = playerDataDir(*world)
	}
	log.Printf("player data directory: %s", playerDir)

	if *remove {
		removeAll(playerDir, *prefix, *count)
		return
	}

	// Positions, indexed the way the runner assigns traces to sessions
	// (round_robin: trace = index % len(traces)), so account N is placed where
	// the trace N will actually replay.
	var origins []*tracefile.Origin
	if *manifest != "" {
		origins = loadOrigins(*manifest)
	}
	fallback := parseOrigin(*originFlag)
	if len(origins) == 0 && fallback == nil {
		log.Fatalf("no positions available: pass --manifest (with traces that carry an origin) or --origin x,y,z")
	}

	dv := int32(*dataVersion)
	if dv == 0 {
		dv = readDataVersion(filepath.Join(*world, "level.dat"))
	}
	log.Printf("DataVersion %d", dv)

	if err := os.MkdirAll(playerDir, 0o755); err != nil {
		log.Fatalf("create %s: %v", playerDir, err)
	}

	written, skipped, inexact := 0, 0, 0
	for i := 0; i < *count; i++ {
		name := fmt.Sprintf("%s%05d", *prefix, i)
		if len(name) > 16 {
			// Matches the runner's truncation, or the file would be written for
			// a username no bot ever logs in as.
			name = name[:16]
		}
		o := fallback
		if len(origins) > 0 {
			if t := origins[i%len(origins)]; t != nil {
				o = t
			}
		}
		if o == nil {
			skipped++
			continue
		}
		if !o.Exact {
			inexact++
		}
		uuid := mcproto.OfflineUUID(name)
		path := filepath.Join(playerDir, formatUUID(uuid)+".dat")
		if *dryRun {
			log.Printf("would write %s -> %s at %.2f %.2f %.2f", name, filepath.Base(path), o.X, o.Y, o.Z)
			written++
			continue
		}
		if err := nbt.Write(path, playerNBT(dv, o, int32(*gameMode), uuid)); err != nil {
			log.Fatalf("write %s: %v", path, err)
		}
		written++
	}

	verb := "wrote"
	if *dryRun {
		verb = "would write"
	}
	log.Printf("%s %d player data file(s) to %s", verb, written, playerDir)
	if skipped > 0 {
		// Loud, because the failure it causes downstream is silent: those bots
		// spawn at world spawn and their block events do nothing.
		log.Printf("WARNING: %d account(s) had no position and were skipped; they will "+
			"spawn at world spawn. Pass --origin x,y,z to place them.", skipped)
	}
	if inexact > 0 {
		log.Printf("note: %d position(s) were inferred rather than captured", inexact)
	}
}

// playerDataDir finds where this server keeps player data.
//
// It moved: current versions use <world>/players/data, older ones
// <world>/playerdata. Getting this wrong fails silently in the worst way —
// the files are written, the server ignores them, every bot spawns at world
// spawn, and nothing is logged anywhere. So prefer a directory that already
// exists, and say which one was chosen.
func playerDataDir(world string) string {
	candidates := []string{
		filepath.Join(world, "players", "data"), // current layout
		filepath.Join(world, "playerdata"),      // pre-reorganisation
	}
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && fi.IsDir() {
			return c
		}
	}
	// Neither exists: the world has never had a player in it. The current
	// layout is the better guess, but say so, because an empty guess is exactly
	// the case that fails silently.
	log.Printf("note: no existing player data directory under %s; assuming %s. "+
		"If bots still spawn at world spawn, start the server once, let a player "+
		"join, and check where it wrote the .dat — then pass --dir.",
		world, candidates[0])
	return candidates[0]
}

// loadOrigins reads every trace in the manifest and returns their origins in
// manifest order, nil where a trace carries none.
func loadOrigins(path string) []*tracefile.Origin {
	man, err := tracefile.LoadManifest(path)
	if err != nil {
		log.Fatalf("load manifest: %v", err)
	}
	base := filepath.Dir(path)
	out := make([]*tracefile.Origin, 0, len(man.Traces))
	have := 0
	for _, entry := range man.Traces {
		t, err := tracefile.Read(filepath.Join(base, entry.File))
		if err != nil {
			log.Fatalf("read trace %s: %v", entry.File, err)
		}
		if t.Origin != nil {
			have++
		}
		out = append(out, t.Origin)
	}
	log.Printf("%d of %d traces carry an origin", have, len(out))
	return out
}

func parseOrigin(s string) *tracefile.Origin {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	if len(parts) != 3 {
		log.Fatalf("--origin must be \"x,y,z\", got %q", s)
	}
	var v [3]float64
	for i, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil {
			log.Fatalf("--origin component %q: %v", p, err)
		}
		v[i] = f
	}
	return &tracefile.Origin{X: v[0], Y: v[1], Z: v[2]}
}

// readDataVersion pulls the world's DataVersion out of level.dat.
//
// Player data without one is run through the datafixers as if it came from an
// ancient version, which mangles it. Taking the number from the world the file
// is going into means it is right by construction, whatever server version this
// is.
func readDataVersion(path string) int32 {
	root, err := nbt.Read(path)
	if err != nil {
		log.Fatalf("read %s: %v (pass --data-version to skip this)", path, err)
	}
	if v, ok := nbt.Int(root, "Data", "DataVersion"); ok {
		return v
	}
	if v, ok := nbt.Int(root, "DataVersion"); ok {
		return v
	}
	log.Fatalf("%s has no DataVersion; pass --data-version", path)
	return 0
}

// playerNBT builds the smallest player data file the server will accept. Every
// field the server can default is left out; what remains is the position, the
// identity, and the handful of vitals that default to zero and would otherwise
// spawn a dead player.
func playerNBT(dataVersion int32, o *tracefile.Origin, gameMode int32, uuid [16]byte) nbt.Compound {
	dim, ok := dimensionNames[o.Dimension]
	if !ok {
		dim = dimensionNames[0]
	}
	return nbt.Compound{
		"DataVersion": dataVersion,
		"Pos":         nbt.List{ElemType: nbt.TagDouble, Items: []any{o.X, o.Y, o.Z}},
		"Motion":      nbt.List{ElemType: nbt.TagDouble, Items: []any{0.0, 0.0, 0.0}},
		"Rotation": nbt.List{ElemType: nbt.TagFloat,
			Items: []any{o.Yaw, o.Pitch}},
		"Dimension":           dim,
		"playerGameType":      gameMode,
		"Health":              float32(20),
		"foodLevel":           int32(20),
		"foodSaturationLevel": float32(5),
		"Air":                 int16(300),
		"Fire":                int16(-20),
		"OnGround":            int8(1),
		"FallDistance":        float32(0),
		"Invulnerable":        int8(0),
		"UUID":                uuidInts(uuid),
		"Inventory":           nbt.List{ElemType: nbt.TagCompound},
		"EnderItems":          nbt.List{ElemType: nbt.TagCompound},
	}
}

// uuidInts encodes a UUID the way entity NBT stores it: four big-endian ints.
func uuidInts(u [16]byte) []int32 {
	out := make([]int32, 4)
	for i := 0; i < 4; i++ {
		out[i] = int32(uint32(u[i*4])<<24 | uint32(u[i*4+1])<<16 |
			uint32(u[i*4+2])<<8 | uint32(u[i*4+3]))
	}
	return out
}

func formatUUID(u [16]byte) string {
	h := hex.EncodeToString(u[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func removeAll(playerDir, prefix string, count int) {
	removed := 0
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("%s%05d", prefix, i)
		if len(name) > 16 {
			name = name[:16]
		}
		path := filepath.Join(playerDir, formatUUID(mcproto.OfflineUUID(name))+".dat")
		if err := os.Remove(path); err == nil {
			removed++
		} else if !os.IsNotExist(err) {
			log.Printf("remove %s: %v", path, err)
		}
	}
	log.Printf("removed %d player data file(s) from %s", removed, playerDir)
}
