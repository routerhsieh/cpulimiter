package movingavg

import "time"

const defaultWindow = 5 * time.Minute

// TimeWindow averages CPU time over the most recent elapsed-time window.
// Observations describe consecutive intervals. Consumption is assumed uniform
// within an interval when trimming its boundary. It is not thread-safe.
// Its zero value is ready to use with a five-minute window.
type TimeWindow struct {
	window time.Duration
	buf    []cpuInterval
	head   int
	count  int
	wall   time.Duration
	cpu    float64 // Nanoseconds; retains fractional CPU at partial boundaries.
}

type cpuInterval struct {
	wall time.Duration
	cpu  float64
}

// NewTimeWindow constructs an empty average. Nonpositive window uses five minutes.
func NewTimeWindow(window time.Duration) *TimeWindow {
	if window <= 0 {
		window = defaultWindow
	}
	return &TimeWindow{window: window}
}

// Add records CPU consumed during a positive wall interval. Negative CPU or
// nonpositive wall time is ignored without changing history. Zero CPU is a valid
// idle interval. A delayed observation spanning the window is prorated.
func (r *TimeWindow) Add(cpu, wall time.Duration) {
	if cpu < 0 || wall <= 0 {
		return
	}
	if r.window <= 0 {
		r.window = defaultWindow
	}
	value := float64(cpu)
	if wall >= r.window {
		r.Reset()
		value *= float64(r.window) / float64(wall)
		wall = r.window
	} else {
		// Trim before adding to avoid overflowing durations near their limit.
		r.trim(r.window - wall)
	}
	if r.count == len(r.buf) {
		buf := make([]cpuInterval, max(16, len(r.buf)*2))
		for i := 0; i < r.count; i++ {
			buf[i] = r.buf[(r.head+i)%len(r.buf)]
		}
		r.buf, r.head = buf, 0
	}
	r.buf[(r.head+r.count)%len(r.buf)] = cpuInterval{wall: wall, cpu: value}
	r.count++
	r.wall += wall
	r.cpu += value
}

// Avg returns retained CPU / retained wall time, or zero if empty. Unobserved
// time does not contribute to the denominator while history is filling.
func (r *TimeWindow) Avg() float64 {
	if r.wall == 0 {
		return 0
	}
	return r.cpu / float64(r.wall)
}

// Resize changes the window, preserving available history. A shrinking window
// prorates its oldest interval; growing cannot recover discarded observations.
// Nonpositive window is ignored, preserving the existing setting and history.
func (r *TimeWindow) Resize(window time.Duration) {
	if window <= 0 {
		return
	}
	r.window = window
	r.trim(window)
}

// Reset discards history but retains allocated storage and the window.
func (r *TimeWindow) Reset() {
	r.head, r.count, r.wall, r.cpu = 0, 0, 0, 0
}

func (r *TimeWindow) trim(wall time.Duration) {
	for r.wall > wall {
		old := &r.buf[r.head]
		excess := r.wall - wall
		if excess < old.wall {
			removed := old.cpu * (float64(excess) / float64(old.wall))
			old.cpu -= removed
			old.wall -= excess
			r.cpu -= removed
			r.wall = wall
			break
		}
		r.wall -= old.wall
		r.cpu -= old.cpu
		r.head = (r.head + 1) % len(r.buf)
		r.count--
	}
	if r.count == 0 {
		r.Reset()
	} else if r.cpu < 0 {
		// Floating-point subtraction can leave a tiny negative residual.
		r.cpu = 0
	}
}
