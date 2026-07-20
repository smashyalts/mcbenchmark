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
	return &s, nil
}
