package cpulimit

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/routerhsieh/cpulimiter/admission"
)

func TestAgingConfigDefaults(t *testing.T) {
	want := AgingConfig{RampDuration: 30 * time.Second, MaxFracBoost: .1}
	if got := (Config{}).withDefaults().Aging; got != want {
		t.Fatalf("defaults=%+v, want %+v", got, want)
	}
	for _, invalid := range []float64{0, -1, math.NaN(), math.Inf(1), math.Inf(-1)} {
		cfg := Config{Aging: AgingConfig{Enabled: true, Delay: -1, RampDuration: -1, MaxFracBoost: invalid}}
		want.Enabled = true
		l := New(cfg, zeroCPUMeter{}, nil)
		if got := l.GetConfig().Aging; got != want {
			t.Fatalf("New defaults=%+v, want %+v", got, want)
		}
		l.SetConfig(cfg)
		if got := (<-l.cfgCh).Aging; got != want {
			t.Fatalf("SetConfig defaults=%+v, want %+v", got, want)
		}
	}
	custom := AgingConfig{Enabled: true, Delay: 5 * time.Second, RampDuration: time.Minute, MaxFracBoost: 2}
	if got := custom.withDefaults(); got != custom {
		t.Fatalf("custom config changed: %+v", got)
	}
}

func TestAgingFrac(t *testing.T) {
	cfg := AgingConfig{Enabled: true, Delay: 5 * time.Second, RampDuration: 30 * time.Second, MaxFracBoost: .2}
	for _, tt := range []struct {
		name string
		frac float64
		wait time.Duration
		want float64
	}{
		{"before delay", .3, time.Second, .3},
		{"at delay", .3, 5 * time.Second, .3},
		{"midpoint", .3, 20 * time.Second, .4},
		{"at maximum", .3, 35 * time.Second, .5},
		{"after maximum", .3, time.Hour, .5},
		{"negative elapsed", .3, -time.Second, .3},
		{"boost capped", .95, time.Hour, 1},
		{"original capped", 2, 0, 1},
		{"zero normalized before boost", 0, time.Hour, .200001},
		{"NaN normalized before boost", math.NaN(), time.Hour, .200001},
		{"infinity normalized before boost", math.Inf(1), time.Hour, .200001},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := agingFrac(tt.frac, tt.wait, cfg); math.Abs(got-tt.want) > 1e-12 {
				t.Fatalf("frac=%g, want %g", got, tt.want)
			}
		})
	}
	cfg.Enabled = false
	if got := agingFrac(.3, time.Hour, cfg); got != .3 {
		t.Fatalf("disabled aging changed fraction: %g", got)
	}
	cfg.Enabled, cfg.Delay = true, 0
	if got := agingFrac(.3, 15*time.Second, cfg); math.Abs(got-.4) > 1e-12 {
		t.Fatalf("zero delay midpoint=%g", got)
	}
}

func TestOriginalFracCappedForBothPolicies(t *testing.T) {
	for _, policy := range []AdmissionPolicy{admission.Deterministic{}, admission.NewPowerCurve(admission.PowerCurveConfig{MinPassProb: 0})} {
		l := New(regressionConfig(), zeroCPUMeter{}, policy)
		l.avgCPUs = 1
		if l.canProceed(2, 1) {
			t.Fatalf("%T admitted above global allowance with frac=2", policy)
		}
		l.avgCPUs = 0
		if !l.canProceed(2, 1) {
			t.Fatalf("%T rejected idle caller with frac=2", policy)
		}
	}
}

// Keep process load stable while Run supplies real periodic wakeups.
type agingFixedAvg struct{ value float64 }

func (*agingFixedAvg) Add(float64)    {}
func (a *agingFixedAvg) Avg() float64 { return a.value }
func (*agingFixedAvg) Resize(int)     {}

type agingObservedPolicy struct {
	inner  AdmissionPolicy
	states chan admission.State
}

func (p *agingObservedPolicy) CanProceed(s admission.State) bool {
	select {
	case p.states <- s:
	default:
	}
	return p.inner.CanProceed(s)
}

func TestAwaitCapacityAgingAndReset(t *testing.T) {
	for _, tt := range []struct {
		name   string
		policy AdmissionPolicy
	}{
		{"deterministic", admission.Deterministic{}},
		{"power curve", admission.NewPowerCurve(admission.PowerCurveConfig{MinPassProb: 0})},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := regressionConfig()
			cfg.Aging = AgingConfig{Enabled: true, RampDuration: 20 * time.Millisecond, MaxFracBoost: .6}
			p := &agingObservedPolicy{inner: tt.policy, states: make(chan admission.State, 1024)}
			l := New(cfg, zeroCPUMeter{}, p)
			l.avg, l.avgCPUs = &agingFixedAvg{value: .25}, .25
			ctx, cancel := context.WithCancel(context.Background())
			controller := startController(t, l, ctx)
			defer func() { cancel(); awaitStopped(t, controller) }()
			for call := 0; call < 2; call++ {
				waitCtx, stop := context.WithTimeout(ctx, time.Second)
				result := make(chan error, 1)
				go func() { result <- l.AwaitCapacity(waitCtx, .2) }()
				select {
				case err := <-result:
					stop()
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(2 * time.Second):
					stop()
					t.Fatal("aging did not release waiter")
				}
				first := <-p.states
				if math.Abs(first.AllowanceCPUs-.14) > 1e-12 {
					t.Fatalf("call %d inherited aging: %+v", call, first)
				}
				boosted := false
				for len(p.states) > 0 {
					s := <-p.states
					boosted = boosted || s.AllowanceCPUs > first.AllowanceCPUs
					if s.AllowanceCPUs > cfg.GlobalFrac {
						t.Fatalf("allowance exceeds cap: %+v", s)
					}
				}
				if !boosted {
					t.Fatal("waiter released without aging")
				}
			}
		})
	}
}

func TestAwaitCapacityLiveAgingUpdate(t *testing.T) {
	cfg := regressionConfig()
	p := &agingObservedPolicy{inner: admission.Deterministic{}, states: make(chan admission.State, 1024)}
	l := New(cfg, zeroCPUMeter{}, p)
	l.avg, l.avgCPUs = &agingFixedAvg{value: .25}, .25
	ctx, cancel := context.WithCancel(context.Background())
	controller := startController(t, l, ctx)
	defer func() { cancel(); awaitStopped(t, controller) }()
	waitCtx, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	result := make(chan error, 1)
	go func() { result <- l.AwaitCapacity(waitCtx, .2) }()
	// Observe a periodic retry with no boost before enabling aging.
	for i := 0; i < 3; i++ {
		select {
		case s := <-p.states:
			if math.Abs(s.AllowanceCPUs-.14) > 1e-12 {
				t.Fatalf("disabled aging boosted: %+v", s)
			}
		case <-time.After(time.Second):
			t.Fatal("missing retry")
		}
	}
	cfg.Aging = AgingConfig{Enabled: true, Delay: time.Millisecond, RampDuration: time.Nanosecond, MaxFracBoost: .6}
	// No further sampling retry will occur during this test. The config broadcast
	// must admit using time already waited, rather than restarting the aging clock.
	cfg.SampleEvery, cfg.Window = time.Hour, time.Hour
	l.SetConfig(cfg)
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("live aging update did not release waiter")
	}
	awaitCondition(t, "aging config applied", func() bool { return l.GetConfig().Aging.Enabled })

	// Even a fully aged call remains blocked above the global allowance.
	l.mu.Lock()
	l.avg, l.avgCPUs = &agingFixedAvg{value: 1}, 1
	l.mu.Unlock()
	blockedCtx, blockedStop := context.WithTimeout(ctx, 30*time.Millisecond)
	defer blockedStop()
	if err := l.AwaitCapacity(blockedCtx, .2); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("above-cap aging result=%v", err)
	}
}
