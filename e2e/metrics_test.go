//go:build e2e && (darwin || linux)

package e2e

import (
	"context"
	"math"
	"testing"
	"time"
)

func TestObservePhaseIncludesStartupWithoutWarmup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	counters := make([]workerCounters, 1)
	p := longPhase("startup", config(0, .7, 5*time.Millisecond), .7)
	started := false
	r, err := observePhase(ctx, p, counters, []float64{1}, func() {
		started = true
		counters[0].chunks.Add(1) // Work at startup must belong to the measured phase.
	})
	if err != nil {
		t.Fatal(err)
	}
	if !started || !r.Complete || p.Warmup != 0 || len(r.Workers) != 1 || r.Workers[0].Chunks != 1 {
		t.Fatalf("startup was excluded from measurement: started=%t phase=%+v result=%+v", started, p, r)
	}
}

func TestRollingMaximum(t *testing.T) {
	for _, tc := range []struct {
		name    string
		samples []cpuSample
		window  time.Duration
		want    float64
	}{
		// Whole-phase average is only 1.5 CPU, but a complete one-second
		// window uses 6 CPU and must be detected by long-case validation.
		{"burst hidden by phase mean", []cpuSample{{0, 0}, {time.Second, 0}, {2 * time.Second, 6 * time.Second}, {4 * time.Second, 6 * time.Second}}, time.Second, 6},
		// The 700ms interval at 2 CPU alone is shorter than the window.
		{"irregular intervals", []cpuSample{{0, 0}, {700 * time.Millisecond, 700 * time.Millisecond}, {1400 * time.Millisecond, 2100 * time.Millisecond}, {2100 * time.Millisecond, 2800 * time.Millisecond}}, time.Second, 1.5},
		{"exact boundary", []cpuSample{{0, 0}, {time.Second, 2500 * time.Millisecond}}, time.Second, 2.5},
		{"incomplete window", []cpuSample{{0, 0}, {500 * time.Millisecond, time.Second}}, time.Second, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := rollingMaximum(tc.samples, tc.window); math.Abs(got-tc.want) > 1e-12 {
				t.Fatalf("rolling max=%g, want %g", got, tc.want)
			}
		})
	}
}
