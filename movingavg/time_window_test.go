package movingavg

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"
)

func TestTimeWindowWeightsElapsedTime(t *testing.T) {
	r := NewTimeWindow(100 * time.Millisecond)
	r.Add(0, 10*time.Millisecond)
	r.Add(900*time.Millisecond, 90*time.Millisecond)
	if got := r.Avg(); got != 9 {
		t.Fatalf("average=%g, want 9 CPU, not the equal-sample average of 5", got)
	}
	// Evict the idle part and half of the busy part, then add idle time.
	r.Add(0, 55*time.Millisecond)
	if got := r.Avg(); got != 4.5 {
		t.Fatalf("partial boundary average=%g, want 4.5", got)
	}
}

func TestTimeWindowResizeAndDelayedObservation(t *testing.T) {
	r := NewTimeWindow(100 * time.Millisecond)
	r.Add(20*time.Millisecond, 20*time.Millisecond)
	r.Add(160*time.Millisecond, 80*time.Millisecond)
	r.Resize(90 * time.Millisecond)
	if got := r.Avg(); math.Abs(got-170.0/90) > 1e-12 {
		t.Fatalf("partial shrink=%g, want %g", got, 170.0/90)
	}
	r.Resize(time.Second)
	if got := r.Avg(); math.Abs(got-170.0/90) > 1e-12 {
		t.Fatalf("growth invented history: %g", got)
	}
	// A missing-read gap longer than the window must not keep old history.
	r.Add(4*time.Second, 2*time.Second)
	if r.Avg() != 2 || r.wall != time.Second {
		t.Fatalf("long interval: average=%g wall=%s", r.Avg(), r.wall)
	}
	r.Reset()
	if r.Avg() != 0 {
		t.Fatal("reset retained CPU history")
	}
	r.Add(0, time.Second)
	if r.Avg() != 0 {
		t.Fatal("idle after reset retained CPU")
	}
}

func TestTimeWindowFractionalCPUAndDurationLimit(t *testing.T) {
	r := NewTimeWindow(time.Nanosecond)
	r.Add(time.Nanosecond, 3*time.Nanosecond)
	if math.Abs(r.Avg()-1.0/3) > 1e-12 {
		t.Fatalf("lost fractional boundary CPU: %g", r.Avg())
	}
	maximum := time.Duration(1<<63 - 1)
	r = NewTimeWindow(maximum)
	r.Add(maximum-1, maximum-1)
	r.Add(maximum-1, maximum-1)
	if r.Avg() != 1 || r.wall != maximum {
		t.Fatalf("duration overflow: avg=%g wall=%s", r.Avg(), r.wall)
	}
}

func TestTimeWindowMatchesTimeIntegral(t *testing.T) {
	// Reference model integrates original intervals by absolute overlap, rather
	// than incrementally mutating CPU values at ring boundaries.
	type interval struct {
		start, end int64
		cpu        float64
	}
	random := rand.New(rand.NewPCG(42, 17))
	window := int64(10000)
	r := NewTimeWindow(time.Duration(window))
	var history []interval
	var clock, retainedStart int64
	for step := 0; step < 10000; step++ {
		if random.IntN(4) == 0 {
			window = int64(1 + random.IntN(20000))
			r.Resize(time.Duration(window))
		} else {
			wall, cpu := int64(1+random.IntN(3000)), int64(random.IntN(30000))
			history = append(history, interval{clock, clock + wall, float64(cpu)})
			clock += wall
			r.Add(time.Duration(cpu), time.Duration(wall))
		}
		retainedStart = max(retainedStart, clock-window)
		var total float64
		for len(history) > 0 && history[0].end <= retainedStart {
			history = history[1:]
		}
		for _, sample := range history {
			overlap := sample.end - max(sample.start, retainedStart)
			total += sample.cpu * float64(overlap) / float64(sample.end-sample.start)
		}
		want := 0.0
		if clock > retainedStart {
			want = total / float64(clock-retainedStart)
		}
		if got := r.Avg(); math.Abs(got-want) > 1e-9*(1+want) {
			t.Fatalf("step %d: got %g want %g", step, got, want)
		}
	}
}

func TestTimeWindowDefaultsAndZeroValue(t *testing.T) {
	for _, window := range []time.Duration{0, -1} {
		r := NewTimeWindow(window)
		if r.window != 5*time.Minute {
			t.Fatalf("invalid window default=%s", r.window)
		}
		r.Add(time.Second, time.Second)
		if r.Avg() != 1 {
			t.Fatal("default window did not retain valid interval")
		}
	}
	var r TimeWindow
	r.Resize(-1)
	r.Add(-1, time.Second)
	if r.Avg() != 0 {
		t.Fatal("empty history changed on invalid input")
	}
	r.Add(time.Second, time.Second)
	if r.window != 5*time.Minute || r.Avg() != 1 {
		t.Fatalf("zero value not initialized: %+v", r)
	}
	r.Reset()
	r.Add(0, time.Second)
	if r.Avg() != 0 {
		t.Fatal("zero CPU was not treated as valid idle time")
	}
}

func TestTimeWindowInvalidInputsPreserveHistory(t *testing.T) {
	for name, mutate := range map[string]func(*TimeWindow){
		"zero resize":     func(r *TimeWindow) { r.Resize(0) },
		"negative resize": func(r *TimeWindow) { r.Resize(-1) },
		"negative CPU":    func(r *TimeWindow) { r.Add(-1, time.Second) },
		"zero wall":       func(r *TimeWindow) { r.Add(time.Second, 0) },
		"negative wall":   func(r *TimeWindow) { r.Add(time.Second, -1) },
	} {
		t.Run(name, func(t *testing.T) {
			r := NewTimeWindow(time.Second)
			r.Add(2*time.Second, time.Second)
			mutate(r)
			if r.Avg() != 2 || r.window != time.Second || r.wall != time.Second {
				t.Fatalf("invalid input changed existing setting/history: %+v", r)
			}
			// Still usable, retaining half the old busy interval and real idle.
			r.Add(0, 500*time.Millisecond)
			if r.Avg() != 1 {
				t.Fatalf("recovery average=%g, want 1", r.Avg())
			}
		})
	}
}
