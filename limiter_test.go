package cpulimit

import (
	"context"
	"errors"
	"math"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"
)

// Run concurrency regressions in a child test process so a deadlocked limiter
// cannot leave goroutines behind or prevent the rest of the suite from running.
func isolatedRegression(t *testing.T) bool {
	t.Helper()
	if os.Getenv("CPULIMITER_REGRESSION") == t.Name() {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^"+t.Name()+"$", "-test.v")
	cmd.Env = append(os.Environ(), "CPULIMITER_REGRESSION="+t.Name())
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("regression subprocess failed (%v):\n%s", err, output)
	}
	return true
}

type zeroCPUMeter struct{}

func (zeroCPUMeter) ProcessCPU() (time.Duration, error) { return 0, nil }

func regressionConfig() Config {
	return Config{Mode: SingleCore, GlobalFrac: 0.70, Window: time.Second,
		SampleEvery: 10 * time.Millisecond, Hysteresis: 0.02}
}

func TestGlobalFracNormalization(t *testing.T) {
	for _, tt := range []struct {
		name        string
		input, want float64
	}{
		{"NaN", math.NaN(), 0.70},
		{"positive infinity", math.Inf(1), 0.70},
		{"negative infinity", math.Inf(-1), 0.70},
		{"zero", 0, 0.70},
		{"negative", -0.05, 0.70},
		{"custom fraction", 0.05, 0.05},
		{"multiple cores in fixed mode", 2, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := regressionConfig()
			cfg.GlobalFrac = tt.input
			l := New(cfg, zeroCPUMeter{}, nil)
			if got := l.GetConfig().GlobalFrac; got != tt.want {
				t.Fatalf("New normalized GlobalFrac to %g, want %g", got, tt.want)
			}
			if l.capCPUs != tt.want {
				t.Fatalf("New cap = %g CPU, want %g CPU", l.capCPUs, tt.want)
			}
			if !l.canProceed(1, 1) {
				t.Fatal("normalized cap should admit idle work")
			}
			// Inspect the queued replacement to verify normalization also applies
			// to runtime updates without depending on controller scheduling.
			l.SetConfig(cfg)
			if got := (<-l.cfgCh).GlobalFrac; got != tt.want {
				t.Fatalf("SetConfig queued GlobalFrac %g, want %g", got, tt.want)
			}
		})
	}
}

// Item 1: a canceled call that cannot pass admission must return instead of
// acquiring the same mutex twice. Set the cap explicitly to isolate item 6.
func TestAwaitCapacityBlockedCallReturnsOnCancellation(t *testing.T) {
	if isolatedRegression(t) {
		return
	}
	l := New(regressionConfig(), zeroCPUMeter{}, nil)
	l.capCPUs, l.avgCPUs = 0.70, 1.0
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := make(chan error, 1)
	go func() { result <- l.AwaitCapacity(ctx, 1) }()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("AwaitCapacity returned %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("AwaitCapacity did not return after cancellation; blocked admission must not deadlock")
	}
}

// Item 2: the constructor advertises defaults, including a usable window.
func TestNewDefaultsWindow(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("New(Config{}, meter, nil) panicked: %v", r)
		}
	}()
	l := New(Config{}, zeroCPUMeter{}, nil)
	cfg := l.GetConfig()
	if cfg.Window <= 0 {
		t.Fatal("default Window must be positive")
	}
	if sampleCapacity(cfg.Window, cfg.SampleEvery) < 1 {
		t.Fatal("default average must have positive capacity")
	}
}

// Supply deterministic averages so this test exercises controller notifications,
// independently of CPU timing, ring arithmetic, and the AwaitCapacity deadlock in item 1.
type notificationAvg struct {
	n       int
	value   float64
	updated chan struct{}
}

func (a *notificationAvg) Add(float64) {
	a.n++
	a.value = 0.69
	if a.n > 1 {
		a.value = 0.675
	}
	select {
	case a.updated <- struct{}{}:
	default:
	}
}
func (a *notificationAvg) Avg() float64 { return a.value }
func (*notificationAvg) Resize(int)     {}

type gatedCPUMeter struct {
	calls   int
	release chan struct{}
}

func (m *gatedCPUMeter) ProcessCPU() (time.Duration, error) {
	m.calls++
	// Baseline, first sample, then wait until the condition waiter is registered.
	if m.calls == 3 {
		<-m.release
	}
	return 0, nil
}

// Item 3: falling from .69 to .675 crosses the .68 admission boundary, but
// changes by less than .02. The waiter must still receive a notification.
func TestRunWakesWaiterAfterSmallEligibleDrop(t *testing.T) {
	if isolatedRegression(t) {
		return
	}
	meter := &gatedCPUMeter{release: make(chan struct{})}
	l := New(regressionConfig(), meter, nil)
	a := &notificationAvg{updated: make(chan struct{}, 16)}
	l.avg = a
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.Run(ctx)
	select {
	case <-a.updated:
	case <-time.After(time.Second):
		t.Fatal("controller did not produce its first sample")
	}
	ready, resumed := make(chan struct{}), make(chan struct{})
	go func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.avgCPUs <= l.capCPUs-l.cfg.Hysteresis {
			close(ready)
			close(resumed)
			return
		}
		close(ready)
		for l.avgCPUs > l.capCPUs-l.cfg.Hysteresis {
			l.cond.Wait()
		}
		close(resumed)
	}()
	<-ready
	close(meter.release)
	select {
	case <-resumed:
	case <-time.After(time.Second):
		l.mu.Lock()
		avg, threshold := l.avgCPUs, l.capCPUs-l.cfg.Hysteresis
		l.mu.Unlock()
		t.Fatalf("waiter remained asleep: average %.3f, admission threshold %.3f", avg, threshold)
	}
}

// Item 4: updates coalesce without needing an active Run consumer, including
// when multiple callers replace the pending update concurrently.
func TestSetConfigConcurrentUpdatesDoNotBlock(t *testing.T) {
	if isolatedRegression(t) {
		return
	}
	old := runtime.GOMAXPROCS(4)
	defer runtime.GOMAXPROCS(old)
	l := New(regressionConfig(), zeroCPUMeter{}, nil)
	l.SetConfig(regressionConfig()) // Fill the pending slot before contention.
	var wg sync.WaitGroup
	start := make(chan struct{})
	for worker := 0; worker < 64; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < 200; i++ {
				l.SetConfig(regressionConfig())
			}
		}()
	}
	close(start)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("concurrent SetConfig calls blocked with no controller consuming updates")
	}
	// Once concurrent calls finish, a final sequential update must be the pending one.
	final := regressionConfig()
	final.GlobalFrac = 0.42
	l.SetConfig(final)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.Run(ctx)
	deadline := time.After(time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if l.GetConfig().GlobalFrac == final.GlobalFrac {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline:
			t.Fatal("controller did not apply the final config update")
		}
	}
}

// Item 5: positive budgets must admit idle work even if the absolute hysteresis
// exceeds the allowance. Inspect admission directly to isolate item 1.
func TestIdleAdmissionWithSmallThreshold(t *testing.T) {
	for _, tt := range []struct {
		name      string
		cap, frac float64
	}{
		{"small global cap", 0.01, 1},
		{"small caller fraction", 0.70, 0.01},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := regressionConfig()
			cfg.GlobalFrac = tt.cap
			l := New(cfg, zeroCPUMeter{}, nil)
			l.capCPUs = computeCapCPUs(cfg)
			if !l.canProceed(tt.frac, 1) {
				t.Fatalf("idle work rejected: allowance=%g CPU, hysteresis=%g CPU", tt.cap*tt.frac, cfg.Hysteresis)
			}
		})
	}
}

// Item 6: the configured cap must be usable before the controller's first tick.
func TestNewInitializesCap(t *testing.T) {
	for _, mode := range []Mode{SingleCore, AllCores} {
		t.Run(strconv.Itoa(int(mode)), func(t *testing.T) {
			cfg := regressionConfig()
			cfg.Mode = mode
			l := New(cfg, zeroCPUMeter{}, nil)
			want := computeCapCPUs(cfg)
			if l.capCPUs != want {
				t.Errorf("initial cap = %g CPU, want %g CPU", l.capCPUs, want)
			}
			if !l.canProceed(1, 1) {
				t.Error("idle work cannot be admitted before the first sampling tick")
			}
		})
	}
}

// Item 7: isolate cancellation ordering from both the uninitialized cap and
// the blocked-path deadlock by making admission otherwise immediately eligible.
func TestAwaitCapacityCanceledContextDoesNotAdmit(t *testing.T) {
	if isolatedRegression(t) {
		return
	}
	l := New(regressionConfig(), zeroCPUMeter{}, nil)
	l.capCPUs = computeCapCPUs(l.cfg)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := l.AwaitCapacity(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("AwaitCapacity with canceled context returned %v, want context.Canceled", err)
	}
}

type initiallyFailingMeter struct {
	calls int
	read  chan struct{}
}

// Observe the entry check and first waiting-loop check without timing sleeps.
// Only the waiter calls Err; the underlying Context handles cancellation safely.
type waitCheckContext struct {
	context.Context
	checks      int
	loopChecked chan struct{}
}

func (c *waitCheckContext) Err() error {
	err := c.Context.Err()
	c.checks++
	if c.checks == 2 {
		close(c.loopChecked)
	}
	return err
}

func TestAwaitCapacityCancellationWinsOverAdmissionAfterWake(t *testing.T) {
	if isolatedRegression(t) {
		return
	}
	l := New(regressionConfig(), zeroCPUMeter{}, nil)
	l.capCPUs, l.avgCPUs = 0.70, 1.0
	root, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &waitCheckContext{Context: root, loopChecked: make(chan struct{})}
	result := make(chan error, 1)
	go func() { result <- l.AwaitCapacity(ctx, 1) }()
	select {
	case <-ctx.loopChecked:
	case err := <-result:
		t.Fatalf("AwaitCapacity returned before cancellation despite exceeding the cap: %v", err)
	case <-time.After(time.Second):
		t.Fatal("AwaitCapacity did not reach its cancellation check in the waiting loop (entry check and deadlock fixes are prerequisites)")
	}

	// Acquiring mu after the loop's check ensures the waiter has released it
	// through cond.Wait. Change both conditions before letting it wake.
	l.mu.Lock()
	l.avgCPUs = 0
	cancel()
	l.cond.Broadcast()
	l.mu.Unlock()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("eligible but canceled waiter returned %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("AwaitCapacity did not return after cancellation and wakeup")
	}
}

func (m *initiallyFailingMeter) ProcessCPU() (time.Duration, error) {
	m.calls++
	select {
	case m.read <- struct{}{}:
	default:
	}
	if m.calls == 1 {
		return 0, errors.New("initial CPU read unavailable")
	}
	// CPU consumed before Run began must not be counted in its first interval.
	// All subsequent readings are identical: no new CPU consumption.
	return 5 * time.Second, nil
}

// Item 8: establish a successful baseline after an initial meter failure.
func TestRunRecoversFromFailedInitialCPURead(t *testing.T) {
	if isolatedRegression(t) {
		return
	}
	meter := &initiallyFailingMeter{read: make(chan struct{}, 16)}
	l := New(regressionConfig(), meter, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { l.Run(ctx); close(done) }()
	deadline := time.After(time.Second)
	for i := 0; i < 4; i++ {
		select {
		case <-meter.read:
		case <-deadline:
			t.Fatal("controller did not recover sufficiently to read CPU again")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("controller did not stop")
	}
	l.mu.Lock()
	avg := l.avgCPUs
	l.mu.Unlock()
	if avg != 0 {
		t.Fatalf("average = %g CPU despite unchanged successful readings; preexisting CPU time was counted as new usage", avg)
	}
}
