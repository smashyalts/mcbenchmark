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
	// RelocationsUnreproduced counts captured teleports too large for a client
	// to claim. A client cannot teleport itself: a position packet 1600 blocks
	// away is exactly what an illegal move looks like, and the server rejects it
	// and rubber-bands. The bot only follows if the benchmark server teleports it
	// too (a replayed command, a portal), so this is the honest count of how far
	// the replay's world diverged from the capture's.
	RelocationsUnreproduced atomic.Int64
	// DigsSent / DigsConfirmed measure whether the world actually changed.
	// Sending a dig proves nothing: the server drops one that is out of range or
	// aimed at air, and "events replayed" counts it regardless. DigsConfirmed
	// counts the block_update packets the server sent back showing the block
	// gone, so a run that broke nothing reads as 0 instead of looking successful.
	DigsSent      atomic.Int64
	DigsConfirmed atomic.Int64
	// PlacesSent / PlacesConfirmed do the same for building. Placement failed
	// the same silent way digs did — capture recorded the block that appeared
	// rather than the one clicked against, so the server was asked to build one
	// block too high, against air — and nothing in the report could show it.
	PlacesSent      atomic.Int64
	PlacesConfirmed atomic.Int64
	// DigStartsSynthesised counts finishes that arrived with no start. Traces
	// recorded from BlockBreakEvent hold nothing else, so replay invents one and
	// the break collapses into a single tick instead of spanning the block's real
	// hardness. A non-zero count means the trace predates packet-level dig
	// capture and its mining load is understated — re-record to fix it.
	DigStartsSynthesised atomic.Int64
	// Attack outcomes. An attack replayed with nothing in range is a swing at
	// air, which reproduces none of combat's cost, so it is counted apart from
	// one that landed. OffType means something was hit but not the species the
	// capture hit — the load is present but not identical.
	AttacksOnType   atomic.Int64
	AttacksOffType  atomic.Int64
	AttacksNoTarget atomic.Int64
	// DigsIntoAir counts digs aimed at a block the client knows is already air.
	// They are not sent, because they are not digs — and before the block ledger
	// existed they were counted as *successes*: the server answers a dig into
	// air with the same block_update(air) it sends when it really broke
	// something. A bot that spawned in the wrong place digs into air almost
	// every time, so the one case the dig counter existed to catch was the one
	// case it got backwards.
	DigsIntoAir atomic.Int64
	// DigsUnverifiable counts digs at positions whose state never arrived —
	// the chunk was not sent, or could not be parsed. Kept apart from both
	// success and failure, because "we could not check" is neither.
	DigsUnverifiable atomic.Int64
	// ChunksUnparsed counts chunk columns this client could not decode, which
	// makes every block in them unverifiable. Non-zero means the chunk format
	// has moved and mcproto/chunk.go needs regenerating against the new version.
	ChunksUnparsed atomic.Int64
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
	ConnectMs        int64  `json:"connect_ms"` // dial -> play ready
	DurationS        int64  `json:"duration_s"` // play ready -> end
	EventsReplayed   int64  `json:"events_replayed"`
	PacketsSent      int64  `json:"packets_sent"`
	TraceLoops       int    `json:"trace_loops"`
	DisconnectReason string `json:"disconnect_reason,omitempty"`
}

// Report is the top-level run output (run.json).
type Report struct {
	Scenario                string          `json:"scenario"`
	Target                  string          `json:"target"`
	Protocol                int             `json:"protocol"`
	StartedAt               time.Time       `json:"started_at"`
	FinishedAt              time.Time       `json:"finished_at"`
	TargetPlayers           int             `json:"target_players"`
	PeakActive              int64           `json:"peak_active"`
	Connected               int64           `json:"sessions_connected"`
	Failed                  int64           `json:"sessions_failed"`
	EventsReplayed          int64           `json:"events_replayed"`
	EventsSkipped           int64           `json:"events_skipped"`
	RelocationsUnreproduced int64           `json:"relocations_unreproduced"`
	DigsSent                int64           `json:"digs_sent"`
	DigsConfirmed           int64           `json:"digs_confirmed"`
	PlacesSent              int64           `json:"places_sent"`
	PlacesConfirmed         int64           `json:"places_confirmed"`
	DigStartsSynthesised    int64           `json:"dig_starts_synthesised"`
	AttacksOnType           int64           `json:"attacks_on_type"`
	AttacksOffType          int64           `json:"attacks_off_type"`
	AttacksNoTarget         int64           `json:"attacks_no_target"`
	DigsIntoAir             int64           `json:"digs_into_air"`
	DigsUnverifiable        int64           `json:"digs_unverifiable"`
	ChunksUnparsed          int64           `json:"chunks_unparsed"`
	PacketsSent             int64           `json:"packets_sent"`
	BytesIn                 int64           `json:"bytes_in"`
	BytesOut                int64           `json:"bytes_out"`
	Samples                 []Sample        `json:"samples"`
	Sessions                []SessionResult `json:"sessions"`
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
		Scenario:                scenarioName,
		Target:                  target,
		Protocol:                protocol,
		StartedAt:               c.start,
		FinishedAt:              time.Now(),
		TargetPlayers:           targetPlayers,
		PeakActive:              c.peak,
		Connected:               c.Agg.Connected.Load(),
		Failed:                  c.Agg.Failed.Load(),
		EventsReplayed:          c.Agg.EventsReplayed.Load(),
		EventsSkipped:           c.Agg.EventsSkipped.Load(),
		RelocationsUnreproduced: c.Agg.RelocationsUnreproduced.Load(),
		DigsSent:                c.Agg.DigsSent.Load(),
		DigsConfirmed:           c.Agg.DigsConfirmed.Load(),
		PlacesSent:              c.Agg.PlacesSent.Load(),
		PlacesConfirmed:         c.Agg.PlacesConfirmed.Load(),
		DigStartsSynthesised:    c.Agg.DigStartsSynthesised.Load(),
		AttacksOnType:           c.Agg.AttacksOnType.Load(),
		AttacksOffType:          c.Agg.AttacksOffType.Load(),
		AttacksNoTarget:         c.Agg.AttacksNoTarget.Load(),
		DigsIntoAir:             c.Agg.DigsIntoAir.Load(),
		DigsUnverifiable:        c.Agg.DigsUnverifiable.Load(),
		ChunksUnparsed:          c.Agg.ChunksUnparsed.Load(),
		PacketsSent:             c.Agg.PacketsSent.Load(),
		BytesIn:                 c.Agg.BytesIn.Load(),
		BytesOut:                c.Agg.BytesOut.Load(),
		Samples:                 c.samples,
		Sessions:                c.sessions,
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
