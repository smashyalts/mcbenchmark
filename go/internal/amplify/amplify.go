// Package amplify synthesizes many varied sessions from a small real capture.
//
// The motivating case: record 5 real players, then replay 1500. Naively cloning
// a trace 300 times produces lockstep clones that all act at the same moment in
// the same place, which is not a realistic load. Amplify rewrites each clone
// along three axes:
//
//	time  — a per-trace start delay plus per-event jitter, so clones desync
//	space — a per-trace block offset applied to absolute coordinates, so clones
//	        act in different places instead of stacking on one chunk
//	values— numeric literals in commands vary (e.g. auction prices), so clones
//	        produce distinct rows/listings rather than identical ones
//
// Usernames need no rewriting: traces reference {SELF}, which the replay client
// expands to each session's own username.
package amplify

import (
	"sort"
	"strconv"
	"strings"

	"mcbench/internal/rawevent"
	"mcbench/internal/tracefile"
)

// Options controls the variation applied to each synthesized trace.
type Options struct {
	StartJitterUs int64 // max random start delay added to every event in a trace
	EventJitterUs int64 // max +/- jitter applied per event
	SpaceSpread   int32 // max +/- block offset applied to absolute coordinates
	VaryPercent   int   // +/- percent variation applied to integer literals in commands (0 = off)
}

// RNG is a small deterministic PRNG (splitmix64) so amplification is
// reproducible from a seed. Not for cryptographic use.
type RNG struct{ state uint64 }

func NewRNG(seed uint64) *RNG { return &RNG{state: seed} }

func (r *RNG) Next() uint64 {
	r.state += 0x9e3779b97f4a7c15
	z := r.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// span returns a value in [-n, n]. Returns 0 when n <= 0.
func (r *RNG) span(n int64) int64 {
	if n <= 0 {
		return 0
	}
	return int64(r.Next()%uint64(2*n+1)) - n
}

// upto returns a value in [0, n]. Returns 0 when n <= 0.
func (r *RNG) upto(n int64) int64 {
	if n <= 0 {
		return 0
	}
	return int64(r.Next() % uint64(n+1))
}

// Trace synthesizes one varied copy of src. traceID names the result.
func Trace(src *tracefile.Trace, traceID string, opt Options, rng *RNG) *tracefile.Trace {
	startDelay := rng.upto(opt.StartJitterUs)
	dx := int32(rng.span(int64(opt.SpaceSpread)))
	dz := int32(rng.span(int64(opt.SpaceSpread)))

	out := &tracefile.Trace{
		SchemaVersion:   tracefile.SchemaVersion,
		ProtocolVersion: src.ProtocolVersion,
		WorldProfileID:  src.WorldProfileID,
		TraceID:         traceID,
		Events:          make([]tracefile.TraceEvent, 0, len(src.Events)),
	}

	for _, e := range src.Events {
		off := e.OffsetUs + startDelay + rng.span(opt.EventJitterUs)
		if off < 0 {
			off = 0
		}
		out.Events = append(out.Events, tracefile.TraceEvent{
			OffsetUs: off,
			Kind:     e.Kind,
			Data:     transformPayload(e.Kind, e.Data, dx, dz, opt, rng),
		})
	}

	// Per-event jitter can reorder events; the replay loop consumes them in
	// array order, so restore a non-decreasing schedule.
	sort.SliceStable(out.Events, func(i, j int) bool {
		return out.Events[i].OffsetUs < out.Events[j].OffsetUs
	})
	// Duration is the source's, shifted by this copy's start delay — not merely
	// the last event's offset. A session's length is how long the player stayed
	// connected, and the replay client sends movement for that whole time even
	// after its last recorded event. Deriving it from the last event silently
	// shortens every amplified session relative to the one it was made from.
	out.DurationUs = src.DurationUs + startDelay
	if n := len(out.Events); n > 0 && out.Events[n-1].OffsetUs > out.DurationUs {
		out.DurationUs = out.Events[n-1].OffsetUs
	}
	return out
}

// transformPayload rewrites the payloads that carry absolute positions or
// numeric values. Payloads it does not understand are passed through unchanged,
// so unknown//future event kinds survive amplification intact.
func transformPayload(kind int32, data []byte, dx, dz int32, opt Options, rng *RNG) []byte {
	switch kind {
	case rawevent.KindDig:
		d, err := rawevent.DecodeDig(data)
		if err != nil {
			return data
		}
		d.X += dx
		d.Z += dz
		return d.Encode()

	case rawevent.KindPlaceBlock:
		p, err := rawevent.DecodePlace(data)
		if err != nil {
			return data
		}
		p.X += dx
		p.Z += dz
		return p.Encode()

	case rawevent.KindInvOpen:
		o, err := rawevent.DecodeInvOpen(data)
		if err != nil || !o.HasPos {
			return data
		}
		o.X += dx
		o.Z += dz
		return o.Encode()

	case rawevent.KindCmd:
		if opt.VaryPercent <= 0 {
			return data
		}
		cmd, err := rawevent.DecodeCmd(data)
		if err != nil {
			return data
		}
		return rawevent.EncodeCmd(VaryNumbers(cmd, opt.VaryPercent, rng))

	default:
		return data
	}
}

// VaryNumbers applies +/- pct variation to whole-number tokens in a command,
// leaving everything else (including the {SELF} placeholder and any word
// containing non-digits) untouched. Values stay >= 1 so prices remain valid.
func VaryNumbers(cmd string, pct int, rng *RNG) string {
	if pct <= 0 {
		return cmd
	}
	fields := strings.Split(cmd, " ")
	for i, f := range fields {
		if f == "" || !isAllDigits(f) {
			continue
		}
		n, err := strconv.ParseInt(f, 10, 64)
		if err != nil || n == 0 {
			continue
		}
		delta := n * int64(pct) / 100
		v := n + rng.span(delta)
		if v < 1 {
			v = 1
		}
		fields[i] = strconv.FormatInt(v, 10)
	}
	return strings.Join(fields, " ")
}

func isAllDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(s) > 0
}
