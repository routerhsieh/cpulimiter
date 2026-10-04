package cpulimit

import (
	"context"
	"errors"
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/routerhsieh/cpulimiter/admission"
	"github.com/routerhsieh/cpulimiter/movingavg"
)

func awaitCondition(t *testing.T, description string, predicate func() bool) {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for !predicate() {
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatalf("timed out waiting for %s", description)
		}
	}
}

func startController(t *testing.T, l *Limiter, ctx context.Context) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() { l.Run(ctx); close(done) }()
	return done
}

func awaitStopped(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("controller did not stop")
	}
}

func TestSampleCapacity(t *testing.T) {
	for _, tt := range []struct {
		name          string
		window, every time.Duration
		want          int
	}{
		{"five minutes", 5 * time.Minute, 100 * time.Millisecond, 3000},
		{"round up", 10 * time.Millisecond, 3 * time.Millisecond, 4},
		{"short window", time.Millisecond, time.Second, 1},
		{"equal", time.Second, time.Second, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := sampleCapacity(tt.window, tt.every); got != tt.want {
				t.Fatalf("capacity=%d, want %d", got, tt.want)
			}
		})
	}
	for _, tt := range []struct {
		name          string
		window, every time.Duration
	}{
		{"zero window", 0, time.Second}, {"negative window", -1, time.Second},
		{"zero interval", time.Second, 0}, {"negative interval", time.Second, -1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected invalid-duration panic")
				}
			}()
			sampleCapacity(tt.window, tt.every)
		})
	}
}

func TestConfigNormalizesNonfiniteHysteresis(t *testing.T) {
	for _, invalid := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		t.Run(fmtFloat(invalid), func(t *testing.T) {
			cfg := regressionConfig()
			cfg.Hysteresis = invalid
			if got := cfg.withDefaults().Hysteresis; got != 0.02 {
				t.Fatalf("normalized Hysteresis=%g, want .02", got)
			}
		})
	}
}

func TestDeterministicAdmissionBoundaries(t *testing.T) {
	for _, tt := range []struct {
		name      string
		avg, frac float64
		want      bool
	}{
		{"idle", 0, 1, true}, {"at boundary", regressionConfig().GlobalFrac - regressionConfig().Hysteresis, 1, true},
		{"above boundary", 0.681, 1, false}, {"at cap", 0.70, 1, false},
		{"half cap eligible", 0.32, 0.5, true}, {"half cap blocked", 0.34, 0.5, false},
		{"zero fraction idle", 0, 0, true}, {"negative fraction idle", 0, -1, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			l := New(regressionConfig(), zeroCPUMeter{}, nil)
			l.avgCPUs = tt.avg
			if got := l.canProceed(tt.frac, 1); got != tt.want {
				t.Fatalf("admission=%v, want %v", got, tt.want)
			}
		})
	}
}

// Endpoint probabilities are exactly zero or one, so these tests are not
// statistical and do not depend on a particular random seed.
func TestProbabilisticAdmissionEndpoints(t *testing.T) {
	for _, tt := range []struct {
		name               string
		avg, weight, floor float64
		want               bool
	}{
		{"below soft", 0.5, 1, 0, true}, {"at hard without floor", 1, 1, 0, false},
		{"above upper boundary", 1.03, 1, 1, false},
		{"floor guarantees pass", 1.01, 1, 1, true},
		{"weight saturates probability", 0.9, 10, 0, true},
		{"negative weight defaults", 0.79, -1, 0, true},
		{"zero weight defaults", 0.79, 0, 0, true},
		{"NaN weight defaults", 0.79, math.NaN(), 0, true},
		{"infinite weight defaults", 0.79, math.Inf(1), 0, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := regressionConfig()
			cfg.GlobalFrac = 1
			policy := admission.NewPowerCurve(admission.PowerCurveConfig{SoftRatio: 0.8, Alpha: 2, MinPassProb: tt.floor})
			l := New(cfg, zeroCPUMeter{}, policy)
			l.avgCPUs = tt.avg
			for i := 0; i < 20; i++ {
				if got := l.canProceed(1, tt.weight); got != tt.want {
					t.Fatalf("admission=%v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestAwaitCapacityDeadlineWhileBlocked(t *testing.T) {
	l := New(regressionConfig(), zeroCPUMeter{}, nil)
	l.avgCPUs = 1
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := l.AwaitCapacity(ctx, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("AwaitCapacity=%v, want deadline exceeded", err)
	}
}

func TestAwaitCapacityResumesWhenUsageFalls(t *testing.T) {
	l := New(regressionConfig(), zeroCPUMeter{}, nil)
	l.avgCPUs = 1
	root, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &waitCheckContext{Context: root, loopChecked: make(chan struct{})}
	result := make(chan error, 1)
	go func() { result <- l.AwaitCapacity(ctx, 1) }()
	select {
	case <-ctx.loopChecked:
	case <-time.After(time.Second):
		t.Fatal("waiter did not block")
	}
	l.mu.Lock()
	l.avgCPUs = 0.1
	l.cond.Broadcast()
	l.mu.Unlock()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("eligible waiter did not resume")
	}
}

func TestRootCancellationStopsControllerAndWorkers(t *testing.T) {
	l := New(regressionConfig(), zeroCPUMeter{}, nil)
	l.avgCPUs = 1
	root, cancel := context.WithCancel(context.Background())
	defer cancel()
	const workers = 8
	results := make(chan error, workers)
	for i := 0; i < workers; i++ {
		ctx := &waitCheckContext{Context: root, loopChecked: make(chan struct{})}
		go func() { results <- l.AwaitCapacity(ctx, 1) }()
		select {
		case <-ctx.loopChecked:
		case <-time.After(time.Second):
			t.Fatal("worker did not block")
		}
	}
	// Hourly sampling keeps the initially blocked state until root cancellation.
	l.mu.Lock()
	l.cfg.SampleEvery = time.Hour
	l.mu.Unlock()
	done := startController(t, l, root)
	cancel()
	awaitStopped(t, done)
	for i := 0; i < workers; i++ {
		select {
		case err := <-results:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("worker returned %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("root cancellation did not release worker")
		}
	}
}

func TestRuntimeResizePreservesHistory(t *testing.T) {
	cfg := regressionConfig()
	cfg.Window, cfg.SampleEvery = 4*time.Hour, time.Hour
	l := New(cfg, zeroCPUMeter{}, nil)
	for _, v := range []float64{1, 2, 3, 4} {
		l.avg.Add(v)
	}
	l.avgCPUs = l.avg.Avg()
	ctx, cancel := context.WithCancel(context.Background())
	done := startController(t, l, ctx)
	t.Cleanup(func() { cancel(); awaitStopped(t, done) })
	cfg.Window = 2 * time.Hour
	l.SetConfig(cfg)
	awaitCondition(t, "window update", func() bool { return l.GetConfig().Window == cfg.Window })
	l.mu.Lock()
	got := l.avgCPUs
	l.avg.Add(5)
	afterAdd := l.avg.Avg()
	l.mu.Unlock()
	if got != 3.5 || afterAdd != 4.5 {
		t.Fatalf("shrink: cached average=%g, after adding=%g; want 3.5 and 4.5", got, afterAdd)
	}
	cfg.SampleEvery = 30 * time.Minute // Grow from two to four retained samples.
	l.SetConfig(cfg)
	awaitCondition(t, "cadence update", func() bool { return l.GetConfig().SampleEvery == cfg.SampleEvery })
	l.mu.Lock()
	got = l.avgCPUs
	l.avg.Add(6)
	l.avg.Add(7)
	afterAdd = l.avg.Avg()
	l.mu.Unlock()
	if got != 4.5 || afterAdd != 5.5 {
		t.Fatalf("grow: average=%g, after adding=%g; want 4.5 and 5.5", got, afterAdd)
	}
}

type cpuReading struct {
	cpu time.Duration
	err error
}
type scriptedMeter struct {
	readings []cpuReading
	calls    int
}

func (m *scriptedMeter) ProcessCPU() (time.Duration, error) {
	i := m.calls
	m.calls++
	if i >= len(m.readings) {
		return 0, errors.New("readings exhausted")
	}
	return m.readings[i].cpu, m.readings[i].err
}

type recordingAvg struct {
	*movingavg.RingAvg
	samples chan float64
}

func (a *recordingAvg) Add(v float64) { a.RingAvg.Add(v); a.samples <- v }

func TestRunMeterFailuresAndCounterRollback(t *testing.T) {
	for _, tt := range []struct {
		name     string
		readings []cpuReading
	}{
		{"intermittent error", []cpuReading{{cpu: time.Second}, {err: errors.New("unavailable")}, {cpu: time.Second}, {cpu: time.Second}}},
		{"counter rollback", []cpuReading{{cpu: 5 * time.Second}, {cpu: time.Second}, {cpu: time.Second}, {cpu: time.Second}}},
		{"negative reading", []cpuReading{{cpu: time.Second}, {cpu: -time.Second}, {cpu: time.Second}, {cpu: time.Second}}},
		{"negative initial reading", []cpuReading{{cpu: -time.Second}, {cpu: time.Second}, {cpu: time.Second}, {cpu: time.Second}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			l := New(regressionConfig(), &scriptedMeter{readings: tt.readings}, nil)
			avg := &recordingAvg{RingAvg: movingavg.New(10), samples: make(chan float64, 16)}
			l.avg = avg
			ctx, cancel := context.WithCancel(context.Background())
			done := startController(t, l, ctx)
			t.Cleanup(func() { cancel(); awaitStopped(t, done) })
			for i := 0; i < 2; i++ {
				select {
				case sample := <-avg.samples:
					if sample != 0 {
						t.Fatalf("sample=%g CPU, want zero for unchanged/reset counter", sample)
					}
				case <-time.After(time.Second):
					t.Fatal("meter recovery did not produce samples")
				}
			}
		})
	}
}

func TestInvalidWaitFractionDoesNotBypassCap(t *testing.T) {
	for _, frac := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		t.Run(fmtFloat(frac), func(t *testing.T) {
			l := New(regressionConfig(), zeroCPUMeter{}, nil)
			l.avgCPUs = 1
			if l.canProceed(frac, 1) {
				t.Fatal("nonfinite fraction bypassed the configured cap")
			}
		})
	}
}

func fmtFloat(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

type failingMeter struct{}

func (failingMeter) ProcessCPU() (time.Duration, error) { return 0, errors.New("meter unavailable") }

func TestMeterFailureKeepsHistoryAndAllowsConfigUpdates(t *testing.T) {
	l := New(regressionConfig(), failingMeter{}, nil)
	l.avg.Add(0.9)
	l.avgCPUs = l.avg.Avg()
	ctx, cancel := context.WithCancel(context.Background())
	done := startController(t, l, ctx)
	t.Cleanup(func() { cancel(); awaitStopped(t, done) })
	cfg := l.GetConfig()
	cfg.GlobalFrac = 0.8
	l.SetConfig(cfg)
	awaitCondition(t, "config application despite meter failure", func() bool { return l.GetConfig().GlobalFrac == 0.8 })
	l.mu.Lock()
	avg, cap := l.avgCPUs, l.capCPUs
	l.mu.Unlock()
	if avg != 0.9 || cap != 0.8 {
		t.Fatalf("average=%g cap=%g, want retained history .9 and new cap .8", avg, cap)
	}
	waitCtx, stopWait := context.WithTimeout(ctx, 20*time.Millisecond)
	defer stopWait()
	if err := l.AwaitCapacity(waitCtx, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked waiter during meter failure returned %v", err)
	}
}

func TestConfigUpdateRaisesCapAndWakesWaiter(t *testing.T) {
	cfg := regressionConfig()
	cfg.SampleEvery, cfg.Window = time.Hour, time.Hour
	l := New(cfg, zeroCPUMeter{}, nil)
	l.avg.Add(0.9)
	l.avgCPUs = 0.9
	root, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := startController(t, l, root)
	t.Cleanup(func() { cancel(); awaitStopped(t, done) })
	ctx := &waitCheckContext{Context: root, loopChecked: make(chan struct{})}
	result := make(chan error, 1)
	go func() { result <- l.AwaitCapacity(ctx, 1) }()
	select {
	case <-ctx.loopChecked:
	case <-time.After(time.Second):
		t.Fatal("waiter did not block")
	}
	cfg.GlobalFrac = 1
	l.SetConfig(cfg)
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("new cap did not admit waiter: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("config update did not wake eligible waiter")
	}
}

func TestConfigDefaultsAndCustomSettings(t *testing.T) {
	got := (Config{Window: -1, SampleEvery: -1, Hysteresis: -1}).withDefaults()
	if got.Window != 5*time.Minute || got.SampleEvery != 100*time.Millisecond ||
		got.Hysteresis != .02 {
		t.Fatalf("unexpected defaults: %+v", got)
	}
	custom := Config{Mode: Mode(99), GlobalFrac: .05, Window: 3 * time.Minute,
		SampleEvery: 50 * time.Millisecond, Hysteresis: .001}
	if got := custom.withDefaults(); got != custom {
		t.Fatalf("valid settings changed: %+v", got)
	}
	if cap := computeCapCPUs(custom); cap != .05 {
		t.Fatalf("non-AllCores mode cap=%g, want .05", cap)
	}
}

func TestAwaitCapacityUsesOnlyFirstOptionalWeight(t *testing.T) {
	cfg := regressionConfig()
	cfg.GlobalFrac = 1
	l := New(cfg, zeroCPUMeter{}, admission.NewPowerCurve(admission.PowerCurveConfig{}))
	l.avgCPUs = .9
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := l.AwaitCapacity(ctx, 1, 10, math.NaN()); err != nil {
		t.Fatalf("first weight should guarantee admission: %v", err)
	}
}
