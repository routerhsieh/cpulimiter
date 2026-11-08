package cpulimit

import (
	"context"
	"math"
	"runtime"
	"testing"
	"time"

	"github.com/routerhsieh/cpulimiter/movingavg"
)

func TestRecordCPUUsesElapsedTimeAcrossGap(t *testing.T) {
	cfg := regressionConfig()
	cfg.Window = 100 * time.Millisecond
	l := New(cfg, zeroCPUMeter{}, nil)
	start := time.Unix(1, 0)
	l.recordCPU(time.Second, start)
	l.recordCPU(time.Second, start.Add(10*time.Millisecond))
	// No successful observations in between: both CPU and wall cover the gap.
	l.recordCPU(1900*time.Millisecond, start.Add(100*time.Millisecond))
	if l.avgCPUs != 9 {
		t.Fatalf("average=%g, want 9 CPU", l.avgCPUs)
	}
}

func TestRecordCPUCounterResetAndNonIncreasingTime(t *testing.T) {
	l := New(regressionConfig(), zeroCPUMeter{}, nil)
	start := time.Unix(1, 0)
	l.recordCPU(10*time.Second, start)
	l.recordCPU(11*time.Second, start.Add(time.Second))
	l.recordCPU(12*time.Second, start.Add(time.Second)) // Invalid timestamp.
	l.recordCPU(13*time.Second, start.Add(2*time.Second))
	if l.avgCPUs != 2 {
		t.Fatalf("invalid timestamp advanced baseline: %g", l.avgCPUs)
	}
	l.recordCPU(time.Second, start.Add(3*time.Second))
	if l.avgCPUs != 0 {
		t.Fatal("counter reset retained old history")
	}
	l.recordCPU(1200*time.Millisecond, start.Add(4*time.Second))
	if math.Abs(l.avgCPUs-.2) > 1e-12 {
		t.Fatalf("new counter epoch average=%g", l.avgCPUs)
	}
}

type delayedReadMeter struct {
	origin time.Time
	calls  int
	fail   bool
}

func (m *delayedReadMeter) ProcessCPU() (time.Duration, error) {
	time.Sleep(20 * time.Millisecond)
	if m.origin.IsZero() {
		m.origin = time.Now()
	}
	m.calls++
	if m.fail && m.calls == 2 {
		return -1, nil
	}
	return time.Since(m.origin), nil // Synthetic constant 1 CPU, measured at read time.
}

func TestRunPairsCounterWithActualReadTime(t *testing.T) {
	for _, fail := range []bool{false, true} {
		cfg := regressionConfig()
		cfg.SampleEvery = time.Millisecond
		l := New(cfg, &delayedReadMeter{fail: fail}, nil)
		avg := &recordingAvg{TimeWindow: movingavg.NewTimeWindow(time.Second), samples: make(chan float64, 16)}
		l.avg = avg
		ctx, cancel := context.WithCancel(context.Background())
		done := startController(t, l, ctx)
		for i := 0; i < 3; i++ {
			select {
			case sample := <-avg.samples:
				if math.Abs(sample-1) > .1 {
					t.Errorf("delayed read rate=%g, want about 1 CPU (failed read=%t)", sample, fail)
				}
			case <-time.After(time.Second):
				t.Error("no delayed-read sample")
			}
		}
		cancel()
		awaitStopped(t, done)
	}
}

func TestRunPreservesSubMicrosecondCPUDelta(t *testing.T) {
	cfg := regressionConfig()
	cfg.SampleEvery = time.Millisecond
	l := New(cfg, &scriptedMeter{readings: []cpuReading{
		{cpu: time.Second},
		{cpu: time.Second + 500*time.Nanosecond},
	}}, nil)
	avg := &recordingAvg{TimeWindow: movingavg.NewTimeWindow(time.Second), samples: make(chan float64, 1)}
	l.avg = avg
	ctx, cancel := context.WithCancel(context.Background())
	done := startController(t, l, ctx)
	t.Cleanup(func() { cancel(); awaitStopped(t, done) })

	select {
	case sample := <-avg.samples:
		if sample <= 0 {
			t.Fatalf("sample=%g CPU, want a positive sample for a 500ns CPU delta", sample)
		}
	case <-time.After(time.Second):
		t.Fatal("controller did not produce a sample")
	}
}

func TestSamplingIntervalByMode(t *testing.T) {
	base := 100 * time.Millisecond
	for _, mode := range []Mode{SingleCore, AllCores, Mode(99)} {
		cfg := regressionConfig()
		cfg.Mode, cfg.SampleEvery = mode, base
		want := base
		if mode == AllCores {
			want /= time.Duration(runtime.NumCPU())
		}
		if got := samplingInterval(cfg); got != want {
			t.Fatalf("mode %d: interval=%v, want %v", mode, got, want)
		}
	}
	if got := samplingInterval(Config{Mode: AllCores, SampleEvery: time.Nanosecond}); got != time.Nanosecond {
		t.Fatalf("sub-nanosecond division must clamp to 1ns, got %v", got)
	}
}

func TestTimeHistoryIndependentOfSamplingCadence(t *testing.T) {
	for _, mode := range []Mode{SingleCore, AllCores} {
		cfg := regressionConfig()
		cfg.Mode, cfg.Window, cfg.SampleEvery = mode, 10*time.Millisecond, 10*time.Millisecond
		l := New(cfg, zeroCPUMeter{}, nil)
		l.avg.Add(time.Millisecond, 2*time.Millisecond)
		l.avg.Add(9*time.Millisecond, 8*time.Millisecond)
		if got := l.avg.Avg(); got != 1 {
			t.Fatalf("mode %d: average=%g, want 1 from actual CPU/time", mode, got)
		}
	}
}

func TestRuntimeModeWindowAndCadenceResize(t *testing.T) {
	cfg := regressionConfig()
	cfg.Window, cfg.SampleEvery = 2*time.Hour, time.Hour
	l := New(cfg, zeroCPUMeter{}, nil)
	avg := movingavg.NewTimeWindow(2 * time.Hour)
	avg.Add(time.Hour, time.Hour)
	avg.Add(2*time.Hour, time.Hour)
	l.avg, l.avgCPUs = avg, avg.Avg()
	ctx, cancel := context.WithCancel(context.Background())
	done := startController(t, l, ctx)
	t.Cleanup(func() { cancel(); awaitStopped(t, done) })
	for _, update := range []struct {
		mode          Mode
		window, every time.Duration
	}{
		{AllCores, 2 * time.Hour, time.Hour},
		{AllCores, 2 * time.Hour, 30 * time.Minute},
		{AllCores, time.Hour, 30 * time.Minute},
		{SingleCore, time.Hour, time.Hour},
	} {
		cfg.Mode, cfg.Window, cfg.SampleEvery = update.mode, update.window, update.every
		l.SetConfig(cfg)
		awaitCondition(t, "mode/window/cadence update", func() bool { return l.GetConfig() == cfg.withDefaults() })
		l.mu.Lock()
		cached := l.avgCPUs
		l.mu.Unlock()
		wantAvg := 1.5
		if cfg.Window == time.Hour {
			wantAvg = 2
		}
		if cached != wantAvg {
			t.Fatalf("history average=%g, want %g", cached, wantAvg)
		}
	}
}

// Signal CPU reads without coupling the test to the limiter's averaging state.
type pulseMeter struct{ read chan struct{} }

func (m *pulseMeter) ProcessCPU() (time.Duration, error) {
	select {
	case m.read <- struct{}{}:
	default:
	}
	return 0, nil
}

func TestAllCoresControllerUsesEffectiveCadence(t *testing.T) {
	if runtime.NumCPU() == 1 {
		t.Skip("cadence scaling requires multiple logical CPUs")
	}
	for _, name := range []string{"initial AllCores", "switch to AllCores", "change base interval"} {
		t.Run(name, func(t *testing.T) {
			cfg := regressionConfig()
			// The base interval is long enough that a ticker ignoring core count
			// cannot produce three samples within the deadline below.
			cfg.SampleEvery = time.Duration(runtime.NumCPU()) * 100 * time.Millisecond
			cfg.Window = cfg.SampleEvery
			if name != "switch to AllCores" {
				cfg.Mode = AllCores
			}
			if name == "change base interval" {
				cfg.SampleEvery = time.Duration(runtime.NumCPU()) * time.Second
				cfg.Window = cfg.SampleEvery
			}
			meter := &pulseMeter{read: make(chan struct{}, 16)}
			l := New(cfg, meter, nil)
			ctx, cancel := context.WithCancel(context.Background())
			done := startController(t, l, ctx)
			t.Cleanup(func() { cancel(); awaitStopped(t, done) })
			select {
			case <-meter.read:
			case <-time.After(time.Second):
				t.Fatal("baseline was not read")
			}
			if name != "initial AllCores" {
				cfg.Mode = AllCores
				cfg.SampleEvery = time.Duration(runtime.NumCPU()) * 100 * time.Millisecond
				l.SetConfig(cfg)
				awaitCondition(t, "AllCores cadence application", func() bool { return l.GetConfig() == cfg.withDefaults() })
			}
			deadline := time.After(500 * time.Millisecond)
			for i := 0; i < 3; i++ {
				select {
				case <-meter.read:
				case <-deadline:
					t.Fatal("controller did not sample at the effective 100ms interval")
				}
			}
		})
	}
}
