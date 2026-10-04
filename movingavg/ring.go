package movingavg

// RingAvg averages the most recent samples using a fixed-capacity ring buffer.
// It is not safe for concurrent use; callers must serialize access.
// Construct a RingAvg with New before use.
type RingAvg struct {
	buf        []float64 // fixed cap = size
	size       int       // window length (N)
	sum        float64   // sum of currently retained values
	count      int       // how many values are currently valid (<= size)
	writeIndex int       // next position to overwrite
}

// New returns a ring-buffer moving average over the last n samples.
// Panics if n <= 0.
func New(n int) *RingAvg {
	if n <= 0 {
		panic("mavg.New: n must be > 0")
	}
	return &RingAvg{
		buf:  make([]float64, n),
		size: n,
	}
}

// Add records v, evicting the oldest observation when the buffer is full.
// Values should be finite; NaN and infinities are not sanitized.
func (r *RingAvg) Add(v float64) {
	if r.count < r.size {
		// still filling the window
		r.buf[r.writeIndex] = v
		r.sum += v
		r.count++
		r.writeIndex++
		if r.writeIndex == r.size {
			r.writeIndex = 0
		}
		return
	}
	// window full: evict the oldest (the slot we’re about to overwrite)
	old := r.buf[r.writeIndex]
	r.sum += v - old
	r.buf[r.writeIndex] = v
	r.writeIndex++
	if r.writeIndex == r.size {
		r.writeIndex = 0
	}
}

// Avg returns the equally weighted average of retained observations, or zero
// when empty. Unfilled capacity does not contribute to the average.
func (r *RingAvg) Avg() float64 {
	if r.count == 0 {
		return 0
	}
	return r.sum / float64(r.count)
}

// Resize changes the capacity to n, preserving the newest samples that fit.
// Growing retains all samples without adding observations to the average.
// Panics if n <= 0. Resizing to the current capacity does nothing.
func (r *RingAvg) Resize(n int) {
	if n <= 0 {
		panic("mavg.Resize: n must be > 0")
	}
	if n == r.size {
		return
	}

	count := r.count
	if count > n {
		count = n
	}
	buf := make([]float64, n)
	var sum float64
	if count > 0 {
		// writeIndex follows the newest sample, including before the ring fills.
		start := (r.writeIndex - count + r.size) % r.size
		for i := 0; i < count; i++ {
			v := r.buf[(start+i)%r.size]
			buf[i] = v
			sum += v
		}
	}

	r.buf = buf
	r.size = n
	r.count = count
	r.sum = sum
	r.writeIndex = count % n
}
