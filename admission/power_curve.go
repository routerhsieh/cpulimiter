package admission

import (
	"math"
	rand "math/rand/v2"
	"sync"
)

// PowerCurveConfig tunes a probabilistic policy. Alpha=1 is linear; Alpha>1
// throttles more aggressively and 0<Alpha<1 less aggressively between boundaries.
type PowerCurveConfig struct {
	SoftRatio   float64 // Between 0 and 1; invalid values default to 0.8.
	Alpha       float64 // Positive finite exponent; invalid values default to 2.
	MinPassProb float64 // 0..1; zero disables the floor; invalid values default to 0.02.
}

func (c PowerCurveConfig) withDefaults() PowerCurveConfig {
	if !finite(c.SoftRatio) || c.SoftRatio <= 0 || c.SoftRatio >= 1 {
		c.SoftRatio = 0.8
	}
	if !finite(c.Alpha) || c.Alpha <= 0 {
		c.Alpha = 2
	}
	if !finite(c.MinPassProb) || c.MinPassProb < 0 || c.MinPassProb > 1 {
		c.MinPassProb = 0.02
	}
	return c
}

// PowerCurve draws admission with probability clamp(x^Alpha * Weight, 0, 1),
// where x falls from 1 to 0 between the soft threshold and nominal allowance.
// It admits immediately below soft-minus-margin and blocks at allowance-plus-
// margin. A positive floor may admit above the nominal allowance within that margin.
// Its zero value uses default settings. All methods are safe for concurrent use,
// including sharing a policy across limiters and updating it while they run.
type PowerCurve struct {
	mu  sync.RWMutex
	cfg PowerCurveConfig
}

// NewPowerCurve constructs a policy with normalized settings.
func NewPowerCurve(cfg PowerCurveConfig) *PowerCurve {
	return &PowerCurve{cfg: cfg.withDefaults()}
}

// Update replaces all curve settings with defaults applied. Existing waiters
// observe the new settings on their next admission attempt, normally at the
// next sampling tick. It does not reset CPU history or notify a limiter directly.
func (p *PowerCurve) Update(cfg PowerCurveConfig) {
	p.mu.Lock()
	p.cfg = cfg.withDefaults()
	p.mu.Unlock()
}

// Config returns a snapshot of the active, normalized curve settings.
func (p *PowerCurve) Config() PowerCurveConfig {
	p.mu.RLock()
	cfg := p.cfg
	p.mu.RUnlock()
	return cfg.withDefaults()
}

// CanProceed evaluates one attempt using a consistent settings snapshot.
func (p *PowerCurve) CanProceed(s State) bool {
	pass := probability(s, p.Config())
	if pass <= 0 {
		return false
	}
	if pass >= 1 {
		return true
	}
	return rand.Float64() < pass
}

func probability(s State, cfg PowerCurveConfig) float64 {
	s, valid := normalizeState(s)
	if !valid {
		return 0
	}
	soft := cfg.SoftRatio * s.AllowanceCPUs
	if s.AverageCPUs <= soft-s.MarginCPUs {
		return 1
	}
	if s.AverageCPUs >= s.AllowanceCPUs+s.MarginCPUs {
		return 0
	}
	denom := s.AllowanceCPUs - soft
	if denom <= 1e-12 {
		if s.AverageCPUs <= s.AllowanceCPUs-s.MarginCPUs {
			return 1
		}
		return 0
	}
	x := math.Max(0, math.Min(1, (s.AllowanceCPUs-s.AverageCPUs)/denom))
	pass := math.Max(0, math.Min(1, math.Pow(x, cfg.Alpha)*s.Weight))
	return math.Max(pass, cfg.MinPassProb)
}
