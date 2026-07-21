// bench-playerdata places benchmark accounts in the world before they connect.
//
// See internal/playerdata for why this is necessary and what it writes; this is
// the flag surface. Run it with the server stopped.
//
//	bench-playerdata --world server/world --manifest traces/manifest.json \
//	    --prefix BENCH_ --count 500
//	bench-playerdata --world server/world --prefix BENCH_ --count 500 --remove
package main

import (
	"flag"
	"log"
	"os"

	"mcbench/internal/playerdata"
)

func main() {
	o := playerdata.Defaults()
	flag.StringVar(&o.World, "world", "", "world directory containing playerdata/ (required)")
	flag.StringVar(&o.Manifest, "manifest", "", "trace manifest; positions come from the traces it lists")
	flag.StringVar(&o.Prefix, "prefix", o.Prefix, "username prefix, must match the scenario's identity.username_prefix")
	flag.IntVar(&o.Count, "count", 0, "number of accounts, must match the scenario's load.target_players (required)")
	flag.StringVar(&o.Origin, "origin", "", "fallback position \"x,y,z\" for traces that carry none")
	flag.IntVar(&o.GameMode, "gamemode", 0, "playerGameType: 0 survival, 1 creative, 2 adventure, 3 spectator")
	flag.IntVar(&o.DataVersion, "data-version", 0, "override the DataVersion (default: read from the world's level.dat)")
	flag.StringVar(&o.Dir, "dir", "", "player data directory (default: auto-detected under --world)")
	flag.StringVar(&o.ImportDir, "import", "", "directory of real players' .dat files to hand to the bots, 1:1 where they can be matched")
	flag.StringVar(&o.PlayerMap, "player-map", "", "\"hash uuid\" lines mapping capture ids to real UUIDs; only needed when the capture was anonymised with a random salt")
	flag.StringVar(&o.SaltHex, "salt-hex", "", "capture salt, if it was not the default all-zero (anonymize_players: false)")
	flag.BoolVar(&o.Remove, "remove", false, "delete the generated files instead of writing them")
	flag.BoolVar(&o.DryRun, "dry-run", false, "report what would be written and exit")
	flag.Parse()

	if o.World == "" || o.Count <= 0 {
		flag.Usage()
		os.Exit(2)
	}
	if err := playerdata.Run(o); err != nil {
		log.Fatal(err)
	}
}
