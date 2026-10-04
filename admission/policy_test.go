package admission

import (
	"math"
	"sync"
	"testing"
)

func TestDeterministic(t *testing.T) {
	for _, tt := range []struct {
		name  string
		state State
		want  bool
	}{
		{"idle", State{AllowanceCPUs: 1, MarginCPUs: .02}, true},
		{"boundary", State{AverageCPUs: 1 - .02, AllowanceCPUs: 1, MarginCPUs: .02}, true},
		{"above boundary", State{AverageCPUs: .99, AllowanceCPUs: 1, MarginCPUs: .02}, false},
		{"tiny allowance", State{AllowanceCPUs: .001, MarginCPUs: .02}, true},
		{"ignore weight", State{AllowanceCPUs: 1, Weight: math.Inf(1)}, true},
		{"invalid average", State{AverageCPUs: math.NaN(), AllowanceCPUs: 1}, false},
		{"negative average", State{AverageCPUs: -1, AllowanceCPUs: 1}, false},
		{"zero allowance", State{}, false},
		{"infinite allowance", State{AllowanceCPUs: math.Inf(1)}, false},
		{"invalid margin", State{AverageCPUs: .9, AllowanceCPUs: 1, MarginCPUs: math.NaN()}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := (Deterministic{}).CanProceed(tt.state); got != tt.want {
				t.Fatalf("CanProceed=%v, want %v", got, tt.want)
			}
		})
	}
}

func TestPowerCurveShapes(t *testing.T) {
	s := State{AverageCPUs: .9, AllowanceCPUs: 1, Weight: 1}
	for _, tt := range []struct {
		name        string
		alpha, want float64
	}{
		{"concave", .5, math.Sqrt(.5)}, {"linear", 1, .5}, {"convex", 2, .25},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := PowerCurveConfig{SoftRatio: .8, Alpha: tt.alpha}.withDefaults()
			if got := probability(s, cfg); math.Abs(got-tt.want) > 1e-12 {
				t.Fatalf("probability=%g, want %g", got, tt.want)
			}
		})
	}
}

func TestPowerCurveWeightsFloorAndBoundaries(t *testing.T) {
	for _, tt := range []struct {
		name                     string
		avg, weight, floor, want float64
	}{
		{"immediate admission", .5, 1, 0, 1},
		{"near soft with margin", .79, 1, 0, 1},
		{"default curve", .9, 1, 0, .25},
		{"half weight", .9, .5, 0, .125},
		{"double weight", .9, 2, 0, .5},
		{"saturated weight", .9, 10, 0, 1},
		{"NaN weight", .9, math.NaN(), 0, .25},
		{"negative weight", .9, -1, 0, .25},
		{"infinite weight", .9, math.Inf(1), 0, .25},
		{"floor", .99, 1, .1, .1},
		{"at allowance", 1, 1, 0, 0},
		{"floor above allowance", 1.01, 1, .1, .1},
		{"upper boundary overrides floor", 1.02, 10, 1, 0},
		{"invalid measurement", math.NaN(), 1, 1, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := PowerCurveConfig{MinPassProb: tt.floor}.withDefaults()
			s := State{AverageCPUs: tt.avg, AllowanceCPUs: 1, MarginCPUs: .02, Weight: tt.weight}
			if got := probability(s, cfg); math.Abs(got-tt.want) > 1e-12 {
				t.Fatalf("probability=%g, want %g", got, tt.want)
			}
		})
	}
	// Tiny thresholds preserve the previous deterministic fallback semantics.
	cfg := (PowerCurveConfig{}).withDefaults()
	if got := probability(State{AverageCPUs: 9e-13, AllowanceCPUs: 1e-12, MarginCPUs: 5e-13}, cfg); got != 0 {
		t.Fatalf("degenerate threshold probability=%g, want 0", got)
	}
}

func TestPowerCurveNormalizesSettings(t *testing.T) {
	for _, invalid := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -1} {
		p := NewPowerCurve(PowerCurveConfig{SoftRatio: invalid, Alpha: invalid, MinPassProb: invalid})
		want := PowerCurveConfig{SoftRatio: .8, Alpha: 2, MinPassProb: .02}
		if got := p.Config(); got != want {
			t.Fatalf("invalid %g normalized to %+v, want %+v", invalid, got, want)
		}
	}
	var zero PowerCurve
	if got := zero.Config(); got != (PowerCurveConfig{SoftRatio: .8, Alpha: 2}) {
		t.Fatalf("zero value config=%+v", got)
	}
	if !zero.CanProceed(State{AllowanceCPUs: 1}) {
		t.Fatal("zero-value policy should admit idle work")
	}
	if zero.CanProceed(State{AverageCPUs: 1, AllowanceCPUs: 1}) {
		t.Fatal("zero-value policy should block at allowance without floor")
	}
}

func TestPowerCurveRuntimeUpdate(t *testing.T) {
	p := NewPowerCurve(PowerCurveConfig{})
	s := State{AverageCPUs: 1, AllowanceCPUs: 1, MarginCPUs: .02}
	if p.CanProceed(s) {
		t.Fatal("expected zero probability before update")
	}
	p.Update(PowerCurveConfig{SoftRatio: .6, Alpha: 1, MinPassProb: 1})
	if !p.CanProceed(s) {
		t.Fatal("updated floor should guarantee admission within upper boundary")
	}
	s.AverageCPUs = 1.03
	if p.CanProceed(s) {
		t.Fatal("update must not override upper blocking boundary")
	}
	if got := p.Config(); got != (PowerCurveConfig{SoftRatio: .6, Alpha: 1, MinPassProb: 1}) {
		t.Fatalf("updated config=%+v", got)
	}
}

func TestPowerCurveConcurrentUpdatesAndDecisions(t *testing.T) {
	p := NewPowerCurve(PowerCurveConfig{})
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				if worker%2 == 0 {
					p.Update(PowerCurveConfig{SoftRatio: .8, Alpha: float64(1 + i%3)})
				}
				p.CanProceed(State{AverageCPUs: .9, AllowanceCPUs: 1, Weight: 1})
				p.Config()
			}
		}(worker)
	}
	wg.Wait()
}
