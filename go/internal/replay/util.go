package replay

import "time"

// rng is a small deterministic PRNG (splitmix64) so runs are reproducible
// without pulling in math/rand global state. Not for cryptographic use.
type rng struct{ state uint64 }

func newRNG(seed uint64) *rng { return &rng{state: seed} }

func (r *rng) next() uint64 {
	r.state += 0x9e3779b97f4a7c15
	z := r.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// rateGate limits how many tokens are handed out per second. wait blocks until
// a token is available or stop is closed.
type rateGate struct {
	interval time.Duration
	next     time.Time
}

func newRateGate(perSecond int) *rateGate {
	if perSecond <= 0 {
		perSecond = 1
	}
	return &rateGate{interval: time.Second / time.Duration(perSecond)}
}

func (g *rateGate) wait(stop <-chan struct{}) {
	now := time.Now()
	if g.next.IsZero() || now.After(g.next) {
		g.next = now.Add(g.interval)
		return
	}
	d := time.Until(g.next)
	g.next = g.next.Add(g.interval)
	select {
	case <-time.After(d):
	case <-stop:
	}
}
