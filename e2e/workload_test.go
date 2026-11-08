//go:build e2e && (darwin || linux)

package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	cpulimit "github.com/routerhsieh/cpulimiter"
	"github.com/routerhsieh/cpulimiter/admission"
	"golang.org/x/sys/unix"
)

// All CPU numbers use logical cores, never percentages of the whole host.
type phase struct {
	Name            string
	Config          cpulimit.Config
	TargetCPUs      float64
	Warmup          time.Duration
	Measure         time.Duration
	Duty            float64 // 0 = idle, 1 = sustained; intermediate values create bursts.
	AllowNoProgress bool    // Background CPU can intentionally exclude a probe.
}

type workload struct {
	Name       string
	Policy     string
	Workers    int
	Fractions  []float64
	ChunkBytes int
	Unlimited  bool
	Background float64 // Uncooperative worker duty, independent of the limiter.
	Phases     []phase
}

type workerStats struct {
	Fraction float64       `json:"fraction"`
	Chunks   uint64        `json:"chunks"`
	Waits    [7]uint64     `json:"wait_histogram"`
	MaxWait  time.Duration `json:"max_wait_ns"`
}

type cpuSample struct {
	Elapsed time.Duration `json:"elapsed_ns"`
	CPU     time.Duration `json:"cpu_ns"`
}

type phaseResult struct {
	Name     string        `json:"name"`
	Target   float64       `json:"target_cpus"`
	Wall     time.Duration `json:"measured_wall_ns"`
	CPU      time.Duration `json:"measured_cpu_ns"`
	Samples  []cpuSample   `json:"samples"`
	Workers  []workerStats `json:"workers"`
	Complete bool          `json:"complete"`
}

type workloadResult struct {
	Name             string        `json:"name"`
	OS               string        `json:"os"`
	Arch             string        `json:"arch"`
	GoVersion        string        `json:"go_version"`
	LogicalCPUs      int           `json:"logical_cpus"`
	GOMAXPROCS       int           `json:"gomaxprocs"`
	Phases           []phaseResult `json:"phases"`
	Canceled         bool          `json:"canceled"`
	Shutdown         time.Duration `json:"shutdown_ns"`
	GoroutinesBefore int           `json:"goroutines_before"`
	GoroutinesAfter  int           `json:"goroutines_after"`
	ExternalWall     time.Duration `json:"external_wall_ns"`
	ExternalCPU      time.Duration `json:"external_cpu_ns"`
}

// Read the OS directly on a separate observation path, never the limiter's
// internal average. The parent also measures total CPU using process wait data.
func processCPU() (time.Duration, error) {
	var r unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &r); err != nil {
		return 0, err
	}
	return time.Duration(r.Utime.Sec+r.Stime.Sec)*time.Second +
		time.Duration(r.Utime.Usec+r.Stime.Usec)*time.Microsecond, nil
}

type workerCounters struct {
	chunks atomic.Uint64
	waits  [7]atomic.Uint64
	max    atomic.Int64
}

func (c *workerCounters) recordWait(d time.Duration) {
	limits := [...]time.Duration{time.Millisecond, 10 * time.Millisecond, 100 * time.Millisecond, time.Second, 5 * time.Second, 30 * time.Second}
	bucket := len(limits)
	for i, limit := range limits {
		if d < limit {
			bucket = i
			break
		}
	}
	c.waits[bucket].Add(1)
	for old := c.max.Load(); int64(d) > old; old = c.max.Load() {
		if c.max.CompareAndSwap(old, int64(d)) {
			break
		}
	}
}

func (c *workerCounters) snapshot(frac float64) workerStats {
	s := workerStats{Fraction: frac, Chunks: c.chunks.Load(), MaxWait: time.Duration(c.max.Load())}
	for i := range s.Waits {
		s.Waits[i] = c.waits[i].Load()
	}
	return s
}

func pause(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func active(ctx context.Context, start time.Time, duty float64) bool {
	const period = 100 * time.Millisecond
	position := time.Since(start) % period
	if duty > 0 && position < time.Duration(float64(period)*duty) {
		return true
	}
	pause(ctx, period-position)
	return false
}

func hashLoop(ctx context.Context, limiter *cpulimit.Limiter, frac float64, chunk int, counters *workerCounters, duty *atomic.Uint64, start time.Time) {
	buf := make([]byte, chunk)
	hash := sha256.New()
	for ctx.Err() == nil {
		// Duty is encoded as thousandths to avoid shared floating-point state.
		if !active(ctx, start, float64(duty.Load())/1000) {
			continue
		}
		if limiter != nil {
			before := time.Now()
			if err := limiter.AwaitCapacity(ctx, frac); err != nil {
				break
			}
			counters.recordWait(time.Since(before))
		}
		_, _ = hash.Write(buf)
		counters.chunks.Add(1)
	}
	// Retain the digest so the workload has an observable computation result.
	runtime.KeepAlive(hash.Sum(nil))
}

func observePhase(ctx context.Context, p phase, counters []workerCounters, fractions []float64, startWork func()) (phaseResult, error) {
	r := phaseResult{Name: p.Name, Target: p.TargetCPUs}
	if p.Warmup > 0 {
		if startWork != nil {
			startWork()
			startWork = nil
		}
		if !pause(ctx, p.Warmup) {
			return r, nil
		}
	}
	before := make([]workerStats, len(counters))
	for i := range counters {
		before[i] = counters[i].snapshot(fractions[i])
	}
	cpu, err := processCPU()
	if err != nil {
		return r, err
	}
	start := time.Now()
	r.Samples = append(r.Samples, cpuSample{CPU: cpu})
	interval := p.Config.Window / 10
	if interval > 100*time.Millisecond {
		interval = 100 * time.Millisecond
	}
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	timer := time.NewTimer(p.Measure)
	defer timer.Stop()
	// With no warmup, establish the independent CPU/time baseline before any
	// controller or worker can run. Startup then belongs to the measured phase.
	if startWork != nil {
		startWork()
	}
	for {
		finished := false
		select {
		case <-ctx.Done():
			finished = true
		case <-timer.C:
			r.Complete, finished = true, true
		case <-ticker.C:
		}
		cpu, err = processCPU()
		if err != nil {
			return r, err
		}
		elapsed := time.Since(start)
		r.Samples = append(r.Samples, cpuSample{Elapsed: elapsed, CPU: cpu})
		if finished {
			r.Wall, r.CPU = elapsed, cpu-r.Samples[0].CPU
			for i := range counters {
				s := counters[i].snapshot(fractions[i])
				s.Chunks -= before[i].Chunks
				for j := range s.Waits {
					s.Waits[j] -= before[i].Waits[j]
				}
				// MaxWait is cumulative since process start; histogram/chunks are
				// deltas for this measured phase. Canceled waits are excluded.
				r.Workers = append(r.Workers, s)
			}
			return r, nil
		}
	}
}

func executeWorkload(ctx context.Context, spec workload, ready string) (workloadResult, error) {
	r := workloadResult{Name: spec.Name, OS: runtime.GOOS, Arch: runtime.GOARCH,
		GoVersion: runtime.Version(), LogicalCPUs: runtime.NumCPU(), GOMAXPROCS: runtime.GOMAXPROCS(0), GoroutinesBefore: runtime.NumGoroutine()}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var limiter *cpulimit.Limiter
	if !spec.Unlimited {
		var policy cpulimit.AdmissionPolicy
		if spec.Policy == "power" {
			policy = admission.NewPowerCurve(admission.PowerCurveConfig{SoftRatio: .8, Alpha: 2, MinPassProb: 0})
		}
		limiter = cpulimit.New(spec.Phases[0].Config, nil, policy)
	}
	var wg sync.WaitGroup
	begin := make(chan struct{})
	launch := func(work func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case <-begin:
				work()
			case <-ctx.Done():
			}
		}()
	}
	if limiter != nil {
		launch(func() { limiter.Run(ctx) })
	}
	start := time.Now()
	var duty atomic.Uint64
	duty.Store(uint64(spec.Phases[0].Duty * 1000))
	counters := make([]workerCounters, spec.Workers)
	fractions := make([]float64, spec.Workers)
	for i := range counters {
		fractions[i] = spec.Fractions[i%len(spec.Fractions)]
		launch(func() {
			hashLoop(ctx, limiter, fractions[i], spec.ChunkBytes, &counters[i], &duty, start)
		})
	}
	if spec.Background > 0 {
		launch(func() {
			var backgroundDuty atomic.Uint64
			backgroundDuty.Store(uint64(spec.Background * 1000))
			hashLoop(ctx, nil, 1, spec.ChunkBytes, &workerCounters{}, &backgroundDuty, start)
		})
	}
	// Always stop and join all workers, even if observation or config setup fails.
	defer func() { cancel(); wg.Wait() }()
	if err := os.WriteFile(ready, []byte("ready"), 0600); err != nil {
		return r, err
	}
	for i, p := range spec.Phases {
		if ctx.Err() != nil {
			break
		}
		if limiter != nil && i > 0 {
			limiter.SetConfig(p.Config)
			deadline := time.Now().Add(2 * time.Second)
			for limiter.GetConfig() != p.Config {
				if time.Now().After(deadline) {
					return r, fmt.Errorf("config update %q was not applied", p.Name)
				}
				if !pause(ctx, time.Millisecond) {
					break
				}
			}
		}
		duty.Store(uint64(p.Duty * 1000))
		var startWork func()
		if i == 0 {
			startWork = func() { close(begin) }
		}
		pr, err := observePhase(ctx, p, counters, fractions, startWork)
		if err != nil {
			return r, err
		}
		r.Phases = append(r.Phases, pr)
	}
	r.Canceled = ctx.Err() != nil
	shutdown := time.Now()
	cancel()
	wg.Wait()
	r.Shutdown = time.Since(shutdown)
	// Allow the AwaitCapacity cancellation helpers to finish after callers exit.
	deadline := time.Now().Add(time.Second)
	for runtime.NumGoroutine() > r.GoroutinesBefore && time.Now().Before(deadline) {
		runtime.Gosched()
		pause(context.Background(), time.Millisecond)
	}
	r.GoroutinesAfter = runtime.NumGoroutine()
	return r, nil
}

// The parent starts the same test executable, selecting only this entry point.
// Every workload has its own process, CPU accounting, limiter, and hard timeout.
func TestWorkloadProcess(t *testing.T) {
	path := os.Getenv("CPULIMITER_E2E_SPEC")
	if path == "" {
		t.Skip("subprocess entry point")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var spec workload
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	r, err := executeWorkload(ctx, spec, os.Getenv("CPULIMITER_E2E_READY"))
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	data, err = json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("CPULIMITER_E2E_RESULT"), data, 0600); err != nil {
		t.Fatal(err)
	}
}
