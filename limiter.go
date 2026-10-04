package cpulimit

import (
	"context"
	"math"
	"runtime"
	"sync"
	"time"

	"github.com/routerhsieh/cpulimiter/admission"
	"github.com/routerhsieh/cpulimiter/movingavg"
)

// Mode controls the cap reference and sampling cadence.
type Mode int

const (
	SingleCore Mode = iota // cap against 1.0 CPU
	AllCores               // cap against NumCPU() CPUs; sample NumCPU() times more often
)

// MovingAvg averages the most recent samples with equal weight.
// The limiter serializes all calls; implementations need not be thread-safe.
type MovingAvg interface {
	// Add inserts one observation.
	Add(v float64)
	// Avg returns the current average (0 if empty).
	Avg() float64
	// Resize changes capacity, preserving the newest samples that fit.
	Resize(n int)
}

// AdmissionPolicy decides whether a single attempt may proceed. The selected
// policy is fixed at construction; an implementation may expose synchronized
// methods for runtime tuning. CanProceed runs under the limiter's mutex: it must
// return promptly, must not block on I/O, and must not call back into the limiter.
// A policy shared by multiple limiters must support concurrent calls.
// Cancellation and periodic retries remain the limiter's responsibility.
type AdmissionPolicy interface {
	CanProceed(admission.State) bool
}

// Config holds live-tunable settings for the limiter. Nonpositive GlobalFrac,
// Window, SampleEvery, and Hysteresis use defaults of 0.70, five minutes,
// 100 milliseconds, and 0.02 CPU respectively. Nonfinite floating-point settings
// (NaN and either infinity) also use their defaults. Algorithm-specific admission
// settings belong to the policy supplied to New, not to Config.
type Config struct {
	// Cap model
	Mode       Mode
	GlobalFrac float64 // e.g., 0.70 (must be >0)
	// Window determines sample capacity as ceil(Window / effective interval).
	// Actual time coverage varies with delays, failed reads, and cadence changes.
	Window time.Duration
	// SampleEvery is the base sampling interval. AllCores divides it by the
	// logical CPU count; other modes use it unchanged. The effective interval
	// is at least one nanosecond to prevent a zero-duration ticker.
	SampleEvery time.Duration // e.g., 50–150ms; 500ms for lower overhead
	// Admission margin passed to the policy, bounded to half the allowance.
	// Custom policies may choose whether to apply it.
	Hysteresis float64 // margin in "CPUs", e.g., 0.02
}

func (c Config) withDefaults() Config {
	out := c
	if out.GlobalFrac <= 0 || math.IsNaN(out.GlobalFrac) || math.IsInf(out.GlobalFrac, 0) {
		out.GlobalFrac = 0.70
	}
	if out.Window <= 0 {
		out.Window = 5 * time.Minute
	}
	if out.SampleEvery <= 0 {
		out.SampleEvery = 100 * time.Millisecond
	}
	if out.Hysteresis <= 0 || math.IsNaN(out.Hysteresis) || math.IsInf(out.Hysteresis, 0) {
		out.Hysteresis = 0.02
	}
	return out
}

// UsageMeter reports cumulative *process* CPU time (user+sys) since start.
// Implemented per-OS in usage_*.go.
type UsageMeter interface {
	ProcessCPU() (time.Duration, error)
}

// Limiter cooperatively limits sustained process CPU usage. Share one instance
// across CPU-intensive workers and call AwaitCapacity between bounded chunks of work.
// Measurement includes all process CPU, but only cooperating work is throttled;
// sampling and concurrent chunks can temporarily overshoot the configured cap.
type Limiter struct {
	// sync/signaling
	mu   sync.Mutex
	cond *sync.Cond

	// live config (guarded by mu)
	cfg Config

	// async config updates (handled inside Run via select)
	cfgCh chan Config
	// Serialize producers so dropping and replacing an update is atomic with
	// respect to other SetConfig calls. Run can consume independently.
	cfgUpdateMu sync.Mutex

	// Process meter and internally constructed sample average.
	meter  UsageMeter
	avg    MovingAvg       // NOT assumed thread-safe; always used under l.mu
	policy AdmissionPolicy // Fixed for this instance; called under l.mu.

	// derived state (guarded by mu)
	avgCPUs float64 // current moving average (in CPUs)
	capCPUs float64 // allowed CPUs (derived from cfg & NumCPU)

	// sampling state (owned by Run goroutine)
	lastCPU time.Duration
	lastAt  time.Time
}

// sampleCapacity rounds up so nominal history coverage is at least window.
func sampleCapacity(window, sampleEvery time.Duration) int {
	if window <= 0 || sampleEvery <= 0 {
		panic("cpulimit: window and sample interval must be positive")
	}
	n := window / sampleEvery
	if window%sampleEvery != 0 {
		n++
	}
	if int(n) <= 0 || time.Duration(int(n)) != n {
		panic("cpulimit: sample capacity exceeds int range")
	}
	return int(n)
}

// samplingInterval returns the effective cadence for a defaulted config.
func samplingInterval(cfg Config) time.Duration {
	interval := cfg.SampleEvery
	if cfg.Mode == AllCores {
		interval /= time.Duration(runtime.NumCPU())
	}
	if interval < time.Nanosecond {
		interval = time.Nanosecond
	}
	return interval
}

// New constructs a limiter with an initially empty ring average and applies
// Config defaults. A nil meter uses the OS meter; a nil policy uses
// admission.Deterministic. Start Run once to sample CPU. To change admission
// behavior at runtime, update the supplied policy through its own API.
func New(cfg Config, meter UsageMeter, policy AdmissionPolicy) *Limiter {
	cfg = cfg.withDefaults()
	if meter == nil {
		meter = defaultUsageMeter()
	}
	if policy == nil {
		policy = admission.Deterministic{}
	}
	l := &Limiter{
		cfg:     cfg,
		meter:   meter,
		policy:  policy,
		avg:     movingavg.New(sampleCapacity(cfg.Window, samplingInterval(cfg))),
		cfgCh:   make(chan Config, 1), // coalesce updates
		capCPUs: computeCapCPUs(cfg),
	}
	l.cond = sync.NewCond(&l.mu)
	return l
}

// SetConfig queues a full configuration replacement with defaults applied.
// Calls are safe concurrently; the latest queued update replaces any pending one.
// Run applies updates asynchronously, resizes the average while preserving the
// newest samples that fit, and wakes waiters. Run must be active to apply updates.
func (l *Limiter) SetConfig(cfg Config) {
	cfg = cfg.withDefaults()
	l.cfgUpdateMu.Lock()
	defer l.cfgUpdateMu.Unlock()
	select {
	case l.cfgCh <- cfg:
	default:
		select { // drop stale then send latest
		case <-l.cfgCh:
		default:
		}
		l.cfgCh <- cfg
	}
}

// GetConfig returns the applied configuration; a pending SetConfig update may
// not yet be reflected in this snapshot.
func (l *Limiter) GetConfig() Config {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cfg
}

// AwaitCapacity blocks until admission succeeds or ctx is canceled. Cancellation is
// checked at entry and before every waiting-loop admission attempt.
// frac scales the global cap: 1.0 uses the full cap and 0.5 uses half.
// Nonpositive or nonfinite frac uses 0.000001. Fractions are eligibility thresholds, not
// independent CPU reservations for workers.
// The default deterministic policy admits at or below frac*cap minus
// min(Hysteresis, frac*cap/2). A supplied policy determines its own rules.
// Only the first optional weight is passed to the policy. Nonpositive or
// nonfinite weights use 1.0; the built-in power curve uses it as a probability
// multiplier, while the deterministic policy ignores it.
func (l *Limiter) AwaitCapacity(ctx context.Context, frac float64, weight ...float64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	w := 1.0
	if len(weight) > 0 && weight[0] > 0 && !math.IsNaN(weight[0]) && !math.IsInf(weight[0], 0) {
		w = weight[0]
	}

	// Fast path avoids allocating cancellation machinery for eligible callers.
	if l.canProceed(frac, w) {
		return nil
	}

	// Tiny canceller: broadcast once on ctx cancel for prompt wake.
	cancelOnce := new(sync.Once)
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			l.mu.Lock()
			cancelOnce.Do(func() { l.cond.Broadcast() })
			l.mu.Unlock()
		case <-done:
		}
	}()
	defer close(done)

	// Cond protocol: check under lock → wait → loop
	l.mu.Lock()
	defer l.mu.Unlock()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if l.canProceedLocked(frac, w) {
			return nil
		}
		l.cond.Wait()
	}
}

// Run is the controller loop. Call once in a goroutine with the application root
// context; workers should use that context or descendants for AwaitCapacity.
// Every recorded sample wakes waiters to retry admission. Failed or negative
// meter readings are skipped; the first valid read establishes a baseline.
// A decreasing CPU counter establishes a new baseline without clearing history.
func (l *Limiter) Run(ctx context.Context) {
	// Initialize sampling baseline.
	l.lastAt = time.Now()
	haveBaseline := false
	if cpu, err := l.meter.ProcessCPU(); err == nil && cpu >= 0 {
		l.lastCPU = cpu
		haveBaseline = true
	}

	// Local ticker cadence that can be changed by config updates.
	cadence := samplingInterval(l.GetConfig())
	tick := time.NewTicker(cadence)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case newCfg := <-l.cfgCh:
			l.mu.Lock()
			old := l.cfg
			newCadence := samplingInterval(newCfg)
			l.cfg = newCfg

			// Recompute cap CPUs under new config.
			l.capCPUs = computeCapCPUs(l.cfg)

			// Preserve recent history when changing the nominal sample window.
			if old.Window != newCfg.Window || cadence != newCadence {
				l.avg.Resize(sampleCapacity(newCfg.Window, newCadence))
			}
			l.avgCPUs = l.avg.Avg()

			// Wake all waiters to re-evaluate under the new config.
			l.cond.Broadcast()
			l.mu.Unlock()

			// Ticker cadence may have changed.
			if cadence != newCadence {
				tick.Reset(newCadence)
				cadence = newCadence
			}

		case now := <-tick.C:
			// Sample process CPU (outside lock).
			cpu, err := l.meter.ProcessCPU()
			if err != nil || cpu < 0 {
				continue
			}
			if !haveBaseline || cpu < l.lastCPU {
				l.lastAt, l.lastCPU = now, cpu
				haveBaseline = true
				continue
			}
			dtWall := now.Sub(l.lastAt)
			if dtWall <= 0 {
				continue
			}
			dtCPU := cpu - l.lastCPU
			l.lastAt, l.lastCPU = now, cpu

			// Instantaneous CPUs over this slice: Δcpu / Δwall (units: "CPUs").
			// Divide durations directly to avoid truncating sub-microsecond values.
			instCPUs := float64(dtCPU) / float64(dtWall)

			// Update the average and cap, then notify waiters.
			l.mu.Lock()
			// Push into the current averaging engine.
			l.avg.Add(instCPUs)
			l.avgCPUs = l.avg.Avg()

			// Update cap each tick (mode may be AllCores).
			l.capCPUs = computeCapCPUs(l.cfg)

			// Even a small decrease can admit a waiter; probabilistic admission
			// also needs retries when the average is unchanged.
			l.cond.Broadcast()
			l.mu.Unlock()
		}
	}
}

// canProceed answers whether the caller may proceed *now*.
// It acquires the limiter lock; use canProceedLocked when already holding it.
func (l *Limiter) canProceed(frac, weight float64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.canProceedLocked(frac, weight)
}

// canProceedLocked evaluates admission. The caller must hold l.mu.
func (l *Limiter) canProceedLocked(frac, weight float64) bool {
	if frac <= 0 || math.IsNaN(frac) || math.IsInf(frac, 0) {
		frac = 0.000001
	}
	if weight <= 0 || math.IsNaN(weight) || math.IsInf(weight, 0) {
		weight = 1.0
	}

	allowance := frac * l.capCPUs
	return l.policy.CanProceed(admission.State{
		AverageCPUs:   l.avgCPUs,
		AllowanceCPUs: allowance,
		MarginCPUs:    math.Min(l.cfg.Hysteresis, allowance/2),
		Weight:        weight,
	})
}

func computeCapCPUs(cfg Config) float64 {
	c := cfg.GlobalFrac
	if cfg.Mode == AllCores {
		c *= float64(runtime.NumCPU())
	}
	if c <= 0 {
		c = 0.001
	}
	return c
}
