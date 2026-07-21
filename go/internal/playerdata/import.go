package playerdata

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"mcbench/internal/nbt"
	"mcbench/internal/tracefile"
)

// importSource is a real player's data file, ready to be handed to a bot.
type importSource struct {
	path string
	uuid [16]byte
	hash string // the capture's anonymised id for this player
}

// loadImportDir reads a directory of real player data files and derives each
// one's capture hash, so traces can be matched back to the player who produced
// them.
//
// The capture log deliberately never stores a raw UUID — it stores
// SHA-256(uuid_bytes || salt). With anonymize_players off that salt is sixteen
// zero bytes, so the hash is reproducible here from the filename alone. With
// anonymisation on the salt is random and discarded, and the only way back is a
// map file the operator exported at capture time.
func loadImportDir(dir string, salt []byte, mapFile string) []importSource {
	byUUID := map[[16]byte]string{}
	if mapFile != "" {
		byUUID = loadPlayerMap(mapFile)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Fatalf("read import dir %s: %v", dir, err)
	}
	var out []importSource
	for _, e := range entries {
		name := e.Name()
		// Only .dat: the server keeps a .dat_old backup beside each file, and
		// taking both would hand two bots the same player twice.
		if e.IsDir() || !strings.HasSuffix(name, ".dat") {
			continue
		}
		u, ok := parseUUID(strings.TrimSuffix(name, ".dat"))
		if !ok {
			continue
		}
		src := importSource{path: filepath.Join(dir, name), uuid: u}
		if h, ok := byUUID[u]; ok {
			src.hash = h
		} else {
			src.hash = hashPlayer(u, salt)
		}
		out = append(out, src)
	}
	log.Printf("import: %d player data file(s) in %s", len(out), dir)
	return out
}

// hashPlayer reproduces CaptureManager.anonymizedId.
func hashPlayer(u [16]byte, salt []byte) string {
	h := sha256.New()
	h.Write(u[:])
	h.Write(salt)
	return hex.EncodeToString(h.Sum(nil))
}

// loadPlayerMap reads "hash<TAB>uuid[<TAB>name]" lines, the export an operator
// makes when the capture was anonymised with a random salt.
func loadPlayerMap(path string) map[[16]byte]string {
	b, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("read player map %s: %v", path, err)
	}
	out := map[[16]byte]string{}
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			log.Fatalf("%s line %d: want \"hash uuid\", got %q", path, i+1, line)
		}
		u, ok := parseUUID(f[1])
		if !ok {
			log.Fatalf("%s line %d: %q is not a UUID", path, i+1, f[1])
		}
		out[u] = strings.ToLower(f[0])
	}
	log.Printf("import: %d entries in player map %s", len(out), path)
	return out
}

func parseUUID(s string) ([16]byte, bool) {
	var u [16]byte
	h := strings.ReplaceAll(s, "-", "")
	if len(h) != 32 {
		return u, false
	}
	b, err := hex.DecodeString(h)
	if err != nil {
		return u, false
	}
	copy(u[:], b)
	return u, true
}

// assignImports pairs each trace with the player data of the player who
// produced it, falling back to leftover files in order.
//
// The exact match is the point: a bot replaying a mining session should carry
// that miner's pickaxe, not an archer's bow. Leftovers keep the fleet's
// *distribution* of gear right when no match is available, which beats
// empty-handed but is not 1:1 — so both counts are reported rather than blurred
// into one success number.
func assignImports(hashes []string, srcs []importSource) []*importSource {
	byHash := map[string]int{}
	for i, s := range srcs {
		if s.hash != "" {
			byHash[s.hash] = i
		}
	}
	used := make([]bool, len(srcs))
	out := make([]*importSource, len(hashes))
	matched := 0
	for i, h := range hashes {
		if h == "" {
			continue
		}
		if j, ok := byHash[strings.ToLower(h)]; ok && !used[j] {
			used[j] = true
			out[i] = &srcs[j]
			matched++
		}
	}
	next, filled := 0, 0
	for i := range out {
		if out[i] != nil {
			continue
		}
		for next < len(srcs) && used[next] {
			next++
		}
		if next >= len(srcs) {
			break
		}
		used[next] = true
		out[i] = &srcs[next]
		filled++
	}
	log.Printf("import: %d trace(s) matched to their own player, %d filled from "+
		"leftovers, %d left empty-handed", matched, filled, len(hashes)-matched-filled)
	if matched == 0 && filled > 0 {
		log.Printf("WARNING: no trace matched its own player, so gear is assigned " +
			"arbitrarily. Exact matching needs either capture.anonymize_players=false " +
			"(the hash is then reproducible from the UUID) or --player-map exported at " +
			"capture time.")
	}
	return out
}

// importedNBT loads a real player's data and rewrites only the fields that must
// belong to the bot rather than to the player it came from.
//
// Everything else is left exactly as the server wrote it — inventory with
// enchantments and durability, ender chest, XP, health, hunger, abilities — which
// is the whole reason to copy the file instead of synthesising one.
func importedNBT(src importSource, o *tracefile.Origin, botUUID [16]byte) (nbt.Compound, error) {
	root, err := nbt.Read(src.path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", src.path, err)
	}
	// The entity UUID must be the bot's, or the data claims to be someone else.
	root["UUID"] = uuidInts(botUUID)
	if o != nil {
		root["Pos"] = nbt.List{ElemType: nbt.TagDouble, Items: []any{o.X, o.Y, o.Z}}
		root["Rotation"] = nbt.List{ElemType: nbt.TagFloat, Items: []any{o.Yaw, o.Pitch}}
		root["Motion"] = nbt.List{ElemType: nbt.TagDouble, Items: []any{0.0, 0.0, 0.0}}
		root["FallDistance"] = float32(0)
		if dim, ok := dimensionNames[o.Dimension]; ok {
			root["Dimension"] = dim
		}
	}
	// A player who logged off dead or dying would give a bot that replays
	// nothing, so revive it rather than silently losing the session.
	if hp, ok := root["Health"].(float32); ok && hp <= 0 {
		root["Health"] = float32(20)
	}
	delete(root, "DeathTime")
	// A vehicle that does not exist in the benchmark world leaves the bot stuck
	// in place for the whole run.
	delete(root, "RootVehicle")
	delete(root, "Passengers")
	return root, nil
}
