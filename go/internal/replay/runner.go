package replay

import (
	"fmt"
	"log"
	"path/filepath"
	"sync"
	"time"

	"mcbench/internal/scenario"
	"mcbench/internal/tracefile"
)

// Runner drives the whole replay: it loads traces, ramps up sessions to the
// target, and tears everything down at the duration limit.
type Runner struct {
	Scenario *scenario.Scenario
	OutDir   string

	traces    []*tracefile.Trace
	traceName []string
	coll      *Collector
	rng       *rng
}

// New builds a Runner, loading the manifest and all trace files it references.
func New(sc *scenario.Scenario, outDir string) (*Runner, error) {
	man, err := tracefile.LoadManifest(sc.Traces.Manifest)
	if err != nil {
		return nil, err
	}
	base := filepath.Dir(sc.Traces.Manifest)
	r := &Runner{Scenario: sc, OutDir: outDir, coll: NewCollector(), rng: newRNG(0x9e3779b97f4a7c15)}
	for _, entry := range man.Traces {
		t, err := tracefile.Read(filepath.Join(base, entry.File))
		if err != nil {
			return nil, fmt.Errorf("load trace %s: %w", entry.File, err)
		}
		r.traces = append(r.traces, t)
		r.traceName = append(r.traceName, entry.File)
	}
	log.Printf("loaded %d traces from %s (protocol %d)", len(r.traces), sc.Traces.Manifest, man.ProtocolVersion)
	return r, nil
}

// Run executes the scenario until the duration limit or all sessions end.
func (r *Runner) Run() error {
	sc := r.Scenario
	target := fmt.Sprintf("%s:%d", sc.Target.Host, sc.Target.Port)
	runDur := time.Duration(sc.Limits.MaxDurationMinutes) * time.Minute
	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Sampler.
	samplerDone := make(chan struct{})
	go func() {
		defer close(samplerDone)
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				r.coll.TakeSample()
				a := &r.coll.Agg
				log.Printf("active=%d connected=%d failed=%d events=%d",
					a.Active.Load(), a.Connected.Load(), a.Failed.Load(), a.EventsReplayed.Load())
			}
		}
	}()

	// Global end timer.
	deadline := time.Now().Add(runDur)
	perSession := time.Duration(sc.Traces.PerSessionMinutes) * time.Minute
	if perSession <= 0 {
		perSession = runDur
	}

	// Ramp: launch InitialPlayers, then AddPerSecond*Interval each interval
	// until TargetPlayers is reached. Connect rate is bounded by a token gate.
	gate := newRateGate(sc.Limits.ConnectPerSecond)
	launched := 0
	launch := func(n int) {
		for i := 0; i < n && launched < sc.Load.TargetPlayers; i++ {
			gate.wait(stop)
			idx := launched
			sess := r.buildSession(idx, target, deadline, perSession)
			launched++
			wg.Add(1)
			go func() {
				defer wg.Done()
				sess.Run(stop)
			}()
		}
	}

	launch(sc.Load.Ramp.InitialPlayers)
	rampTicker := time.NewTicker(time.Duration(sc.Load.Ramp.IntervalSeconds) * time.Second)
	rampStep := sc.Load.Ramp.AddPerSecond * sc.Load.Ramp.IntervalSeconds
	if rampStep <= 0 {
		rampStep = sc.Load.TargetPlayers // no ramp: go straight to target
	}

rampLoop:
	for launched < sc.Load.TargetPlayers {
		select {
		case <-rampTicker.C:
			launch(rampStep)
		case <-time.After(time.Until(deadline)):
			break rampLoop
		}
	}
	rampTicker.Stop()
	log.Printf("ramp complete: launched %d sessions, target %d", launched, sc.Load.TargetPlayers)

	// Finish when every session has ended, or at the run deadline — whichever
	// comes first.
	//
	// max_duration_minutes is a ceiling, not a target. Waiting for it
	// unconditionally meant a 16-second capture replayed with reuse_policy
	// "once" still blocked for the full limit with nothing connected, which
	// reads as a hang.
	drained := make(chan struct{})
	go func() { wg.Wait(); close(drained) }()
	select {
	case <-drained:
		log.Printf("all sessions finished; draining")
	case <-time.After(time.Until(deadline)):
		log.Printf("duration limit reached (%s); draining", runDur)
	}
	close(stop)

	select {
	case <-drained:
	case <-time.After(15 * time.Second):
		log.Printf("drain timeout; some sessions did not close cleanly")
	}
	<-samplerDone
	r.coll.TakeSample()

	if err := r.coll.WriteReport(r.OutDir, sc.Name, target, sc.Protocol.Version, sc.Load.TargetPlayers); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	log.Printf("report written to %s", filepath.Join(r.OutDir, "run.json"))
	return nil
}

func (r *Runner) buildSession(idx int, target string, deadline time.Time, perSession time.Duration) *Session {
	sc := r.Scenario
	var tIdx int
	switch sc.Traces.Selection.Strategy {
	case "random":
		tIdx = int(r.rng.next() % uint64(len(r.traces)))
	default: // round_robin
		tIdx = idx % len(r.traces)
	}
	tr := r.traces[tIdx]

	// PlayFor is capped so a session never outlives the run deadline.
	playFor := perSession
	if until := time.Until(deadline); until < playFor {
		playFor = until
	}
	// Jitter reuse so looped traces desynchronize.
	if sc.Traces.ReusePolicy == "allow_with_jitter" {
		jitter := time.Duration(r.rng.next()%2000) * time.Millisecond
		playFor += jitter
	}

	username := fmt.Sprintf("%s%05d", sc.Identity.UsernamePrefix, idx)
	if len(username) > 16 {
		username = username[:16] // Minecraft username limit
	}
	return &Session{
		ID:           fmt.Sprintf("s%05d", idx),
		Username:     username,
		Trace:        tr,
		Target:       target,
		Host:         sc.Target.Host,
		Port:         uint16(sc.Target.Port),
		Protocol:     int32(sc.Protocol.Version),
		PlayFor:      playFor,
		EnableFlight: sc.Client.EnableFlight,
		LoopTrace:    sc.Traces.ReusePolicy != "once",
		agg:          &r.coll.Agg,
		coll:         r.coll,
	}
}
