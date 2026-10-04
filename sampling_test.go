package cpulimit

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/routerhsieh/cpulimiter/movingavg"
)

func TestRunPreservesSubMicrosecondCPUDelta(t *testing.T) {
	cfg := regressionConfig()
	cfg.SampleEvery = time.Millisecond
	l := New(cfg, &scriptedMeter{readings: []cpuReading{
		{cpu: time.Second},
		{cpu: time.Second + 500*time.Nanosecond},
	}}, nil)
	avg := &recordingAvg{RingAvg: movingavg.New(1), samples: make(chan float64, 1)}
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

func TestNewRingCapacityUsesEffectiveInterval(t *testing.T) {
	for _, mode := range []Mode{SingleCore, AllCores} {
		cfg := regressionConfig()
		cfg.Mode = mode
		cfg.Window, cfg.SampleEvery = 10*time.Millisecond, 10*time.Millisecond
		expected := sampleCapacity(cfg.Window, samplingInterval(cfg))
		l := New(cfg, zeroCPUMeter{}, nil)
		// Feed one more sample than capacity and check that exactly the oldest
		// sample is evicted, without exposing the ring's internal fields.
		for i := 1; i <= expected+1; i++ {
			l.avg.Add(float64(i))
		}
		want := float64(expected+3) / 2
		if got := l.avg.Avg(); got != want {
			t.Fatalf("mode %d: average=%g, want %g for capacity %d", mode, got, want, expected)
		}
	}
}

type resizeTrackingAvg struct {
	*movingavg.RingAvg
	capacity int // Accessed only under the limiter's mutex.
}

func (a *resizeTrackingAvg) Resize(n int) { a.RingAvg.Resize(n); a.capacity = n }

func TestRuntimeModeWindowAndCadenceResize(t *testing.T) {
	cfg := regressionConfig()
	cfg.Window, cfg.SampleEvery = 2*time.Hour, time.Hour
	l := New(cfg, zeroCPUMeter{}, nil)
	avg := &resizeTrackingAvg{RingAvg: movingavg.New(2), capacity: 2}
	avg.Add(1)
	avg.Add(2)
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
		awaitCondition(t, "mode/window/cadence update", func() bool { return l.GetConfig() == cfg })
		wantCapacity := sampleCapacity(cfg.Window, samplingInterval(cfg))
		l.mu.Lock()
		capacity, cached := avg.capacity, l.avgCPUs
		l.mu.Unlock()
		if capacity != wantCapacity {
			t.Fatalf("capacity=%d, want %d for %+v", capacity, wantCapacity, cfg)
		}
		wantAvg := 1.5
		if wantCapacity == 1 {
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
				awaitCondition(t, "AllCores cadence application", func() bool { return l.GetConfig() == cfg })
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
