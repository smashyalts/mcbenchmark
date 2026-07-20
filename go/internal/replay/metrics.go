package replay

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// Aggregate holds run-wide counters, updated atomically by sessions.
type Aggregate struct {
	Connected      atomic.Int64 // sessions that reached play state
	Active         atomic.Int64 // currently in play state
	Failed         atomic.Int64 // sessions that never reached play state
	Disconnected   atomic.Int64 // sessions disconnected after play
	EventsReplayed atomic.Int64
	PacketsSent    atomic.Int64
	BytesIn        atomic.Int64
	BytesOut       atomic.Int64
	EventsSkipped  atomic.Int64 // trace events with no replay mapping
}

// Sample is one point of the concurrency time series.
type Sample struct {
	TSeconds  int64 `json:"t_s"`
	Active    int64 `json:"active"`
	Connected int64 `json:"connected_total"`
	Failed    int64 `json:"failed_total"`
	Events    int64 `json:"events_replayed_total"`
	Packets   int64 `json:"packets_sent_total"`
}

// SessionResult is the per-session summary written into the run report.
type SessionResult struct {
	ID               string `json:"id"`
	Username         string `json:"username"`
	TraceFile        string `json:"trace_file"`
	State            string `json:"final_state"`
	ConnectMs        int64  `json:"connect_ms"`   // dial -> play ready
	DurationS        int64  `json:"duration_s"`   // play ready -> end
	EventsReplayed   int64  `json:"events_replayed"`
	PacketsSent      int64  `json:"packets_sent"`
	TraceLoops       int    `json:"trace_loops"`
	DisconnectReason string `json:"disconnect_reason,omitempty"`
}

// Report is the top-level run output (run.json).
type Report struct {
	Scenario       string          `json:"scenario"`
	Target         string          `json:"target"`
	Protocol       int             `json:"protocol"`
	StartedAt      time.Time       `json:"started_at"`
	FinishedAt     time.Time       `json:"finished_at"`
	TargetPlayers  int             `json:"target_players"`
	PeakActive     int64           `json:"peak_active"`
	Connected      int64           `json:"sessions_connected"`
	Failed         int64           `json:"sessions_failed"`
	EventsReplayed int64           `json:"events_replayed"`
	EventsSkipped  int64           `json:"events_skipped"`
	PacketsSent    int64           `json:"packets_sent"`
	BytesIn        int64           `json:"bytes_in"`
	BytesOut       int64           `json:"bytes_out"`
	Samples        []Sample        `json:"samples"`
	Sessions       []SessionResult `json:"sessions"`
}

// Collector gathers samples and session results.
type Collector struct {
	Agg Aggregate

	mu       sync.Mutex
	samples  []Sample
	sessions []SessionResult
	peak     int64
	start    time.Time
}

func NewCollector() *Collector { return &Collector{start: time.Now()} }

func (c *Collector) TakeSample() {
	active := c.Agg.Active.Load()
	c.mu.Lock()
	defer c.mu.Unlock()
	if active > c.peak {
		c.peak = active
	}
	c.samples = append(c.samples, Sample{
		TSeconds:  int64(time.Since(c.start).Seconds()),
		Active:    active,
		Connected: c.Agg.Connected.Load(),
		Failed:    c.Agg.Failed.Load(),
		Events:    c.Agg.EventsReplayed.Load(),
		Packets:   c.Agg.PacketsSent.Load(),
	})
}

func (c *Collector) AddSession(r SessionResult) {
	c.mu.Lock()
	c.sessions = append(c.sessions, r)
	c.mu.Unlock()
}

// WriteReport writes run.json (and a small prometheus-style summary) to dir.
func (c *Collector) WriteReport(dir, scenarioName, target string, protocol, targetPlayers int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	rep := Report{
		Scenario:       scenarioName,
		Target:         target,
		Protocol:       protocol,
		StartedAt:      c.start,
		FinishedAt:     time.Now(),
		TargetPlayers:  targetPlayers,
		PeakActive:     c.peak,
		Connected:      c.Agg.Connected.Load(),
		Failed:         c.Agg.Failed.Load(),
		EventsReplayed: c.Agg.EventsReplayed.Load(),
		EventsSkipped:  c.Agg.EventsSkipped.Load(),
		PacketsSent:    c.Agg.PacketsSent.Load(),
		BytesIn:        c.Agg.BytesIn.Load(),
		BytesOut:       c.Agg.BytesOut.Load(),
		Samples:        c.samples,
		Sessions:       c.sessions,
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(&rep, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "run.json"), append(b, '\n'), 0o644); err != nil {
		return err
	}
	prom := "# HELP mcbench_peak_active Peak concurrent play-state sessions\n" +
		"# TYPE mcbench_peak_active gauge\n"
	prom += promLine("mcbench_peak_active", rep.PeakActive)
	prom += promLine("mcbench_sessions_connected_total", rep.Connected)
	prom += promLine("mcbench_sessions_failed_total", rep.Failed)
	prom += promLine("mcbench_events_replayed_total", rep.EventsReplayed)
	prom += promLine("mcbench_packets_sent_total", rep.PacketsSent)
	prom += promLine("mcbench_bytes_out_total", rep.BytesOut)
	prom += promLine("mcbench_bytes_in_total", rep.BytesIn)
	return os.WriteFile(filepath.Join(dir, "metrics.prom"), []byte(prom), 0o644)
}

func promLine(name string, v int64) string {
	b, _ := json.Marshal(v)
	return name + " " + string(b) + "\n"
}
