// Package scenario parses replay scenario YAML files.
package scenario

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Scenario struct {
	Name string `yaml:"name"`

	Target struct {
		Host string `yaml:"host"`
		Port int    `yaml:"port"`
	} `yaml:"target"`

	Protocol struct {
		Version int `yaml:"version"`
	} `yaml:"protocol"`

	Traces struct {
		Manifest  string `yaml:"manifest"`
		Selection struct {
			Strategy string `yaml:"strategy"` // round_robin | random
		} `yaml:"selection"`
		PerSessionMinutes int    `yaml:"per_session_minutes"`
		ReusePolicy       string `yaml:"reuse_policy"` // allow_with_jitter | once
	} `yaml:"traces"`

	Load struct {
		TargetPlayers int `yaml:"target_players"`
		Ramp          struct {
			InitialPlayers  int `yaml:"initial_players"`
			AddPerSecond    int `yaml:"add_per_second"`
			IntervalSeconds int `yaml:"interval_seconds"`
		} `yaml:"ramp"`
	} `yaml:"load"`

	Limits struct {
		MaxDurationMinutes int `yaml:"max_duration_minutes"`
		ConnectPerSecond   int `yaml:"connect_per_second"`
	} `yaml:"limits"`

	Identity struct {
		UsernamePrefix string `yaml:"username_prefix"`
		ReusePolicy    string `yaml:"reuse_policy"`
	} `yaml:"identity"`

	Client struct {
		// EnableFlight sends player_abilities(flying) once play begins. Requires
		// the server to permit flight (creative or allow-flight); leave false for
		// survival benchmarks or the server will kick for illegal flight.
		EnableFlight bool `yaml:"enable_flight"`
	} `yaml:"client"`

	Output struct {
		Dir string `yaml:"dir"`
	} `yaml:"output"`
}

func Load(path string) (*Scenario, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Scenario
	if err := yaml.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	// Defaults.
	if s.Target.Port == 0 {
		s.Target.Port = 25565
	}
	if s.Protocol.Version == 0 {
		s.Protocol.Version = 775
	}
	if s.Traces.Selection.Strategy == "" {
		s.Traces.Selection.Strategy = "round_robin"
	}
	if s.Traces.ReusePolicy == "" {
		s.Traces.ReusePolicy = "allow_with_jitter"
	}
	if s.Load.TargetPlayers == 0 {
		s.Load.TargetPlayers = 1
	}
	if s.Load.Ramp.InitialPlayers == 0 {
		s.Load.Ramp.InitialPlayers = s.Load.TargetPlayers
	}
	if s.Load.Ramp.IntervalSeconds == 0 {
		s.Load.Ramp.IntervalSeconds = 5
	}
	if s.Limits.MaxDurationMinutes == 0 {
		s.Limits.MaxDurationMinutes = 60
	}
	if s.Limits.ConnectPerSecond == 0 {
		s.Limits.ConnectPerSecond = 20
	}
	if s.Identity.UsernamePrefix == "" {
		s.Identity.UsernamePrefix = "BENCH_"
	}
	// Validation.
	if s.Target.Host == "" {
		return nil, fmt.Errorf("%s: target.host is required", path)
	}
	if s.Traces.Manifest == "" {
		return nil, fmt.Errorf("%s: traces.manifest is required", path)
	}
	switch s.Traces.Selection.Strategy {
	case "round_robin", "random":
	default:
		return nil, fmt.Errorf("%s: unknown selection strategy %q", path, s.Traces.Selection.Strategy)
	}
	// Usernames are prefix + a 5-digit index, and Minecraft caps them at 16
	// characters. A longer prefix used to be truncated silently, which does not
	// merely rename the bots — it collapses them onto one name. Every session
	// then logs in as the same player, so the server kicks each new one as a
	// duplicate login and the run reports a wall of failed sessions with no
	// stated cause. bench-playerdata truncates identically, so it also writes a
	// single player data file for accounts that no longer have distinct names.
	if n := len(s.Identity.UsernamePrefix); n > 11 {
		return nil, fmt.Errorf("%s: identity.username_prefix %q is %d characters; "+
			"the 5-digit account index leaves room for 11 (Minecraft's limit is 16)",
			path, s.Identity.UsernamePrefix, n)
	}
	// Negative counts read as "unlimited" nowhere and produce a run that
	// connects nobody while reporting success.
	for _, f := range []struct {
		name string
		v    int
	}{
		{"load.target_players", s.Load.TargetPlayers},
		{"load.ramp.initial_players", s.Load.Ramp.InitialPlayers},
		{"load.ramp.add_per_second", s.Load.Ramp.AddPerSecond},
		{"load.ramp.interval_seconds", s.Load.Ramp.IntervalSeconds},
		{"limits.max_duration_minutes", s.Limits.MaxDurationMinutes},
		{"limits.connect_per_second", s.Limits.ConnectPerSecond},
		{"traces.per_session_minutes", s.Traces.PerSessionMinutes},
	} {
		if f.v < 0 {
			return nil, fmt.Errorf("%s: %s must not be negative (got %d)", path, f.name, f.v)
		}
	}
	return &s, nil
}
