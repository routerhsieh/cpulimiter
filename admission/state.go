// Package admission provides policies for CPU-work admission decisions.
package admission

import "math"

// State is a value snapshot for one admission attempt. CPU values use units of
// cores: 1.0 means one fully occupied logical CPU. It contains runtime inputs,
// not persistent policy configuration.
type State struct {
	AverageCPUs   float64 // Process-wide moving average.
	AllowanceCPUs float64 // Current global cap multiplied by the caller's fraction.
	MarginCPUs    float64 // Limiter's admission margin, bounded to half the allowance.
	Weight        float64 // Optional probability multiplier; invalid values use 1.
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// Invalid measurements or allowances fail closed. Normalize optional inputs so
// the built-in policies are also safe to use outside the limiter.
func normalizeState(s State) (State, bool) {
	if !finite(s.AverageCPUs) || s.AverageCPUs < 0 || !finite(s.AllowanceCPUs) || s.AllowanceCPUs <= 0 {
		return s, false
	}
	if !finite(s.MarginCPUs) || s.MarginCPUs < 0 {
		s.MarginCPUs = 0
	}
	s.MarginCPUs = math.Min(s.MarginCPUs, s.AllowanceCPUs/2)
	if !finite(s.Weight) || s.Weight <= 0 {
		s.Weight = 1
	}
	return s, true
}
