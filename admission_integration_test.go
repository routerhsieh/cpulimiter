package cpulimit_test

import (
	"context"
	"errors"
	"math"
	"sync/atomic"
	"testing"
	"time"

	cpulimit "github.com/routerhsieh/cpulimiter"
	"github.com/routerhsieh/cpulimiter/admission"
)

type idleMeter struct{}

func (idleMeter) ProcessCPU() (time.Duration, error) { return 0, nil }

// A consumer-owned implementation imports only the public state and provides
// its own runtime update behavior; it does not require a built-in policy type.
type customGate struct {
	allow  atomic.Bool
	states chan admission.State
}

func (p *customGate) CanProceed(s admission.State) bool {
	select {
	case p.states <- s:
	default:
	}
	return p.allow.Load()
}

var _ cpulimit.AdmissionPolicy = (*customGate)(nil)
var _ cpulimit.AdmissionPolicy = admission.Deterministic{}
var _ cpulimit.AdmissionPolicy = (*admission.PowerCurve)(nil)

func TestCustomAdmissionPolicyReceivesRuntimeState(t *testing.T) {
	p := &customGate{states: make(chan admission.State, 4)}
	p.allow.Store(true)
	l := cpulimit.New(cpulimit.Config{GlobalFrac: .1, Hysteresis: .01}, idleMeter{}, p)
	if err := l.AwaitCapacity(context.Background(), .5, 2); err != nil {
		t.Fatal(err)
	}
	s := <-p.states
	if s.AverageCPUs != 0 || s.AllowanceCPUs != .05 || s.MarginCPUs != .01 || s.Weight != 2 {
		t.Fatalf("unexpected runtime snapshot: %+v", s)
	}
	if err := l.AwaitCapacity(context.Background(), math.NaN(), math.Inf(1)); err != nil {
		t.Fatal(err)
	}
	s = <-p.states
	if s.Weight != 1 || s.AllowanceCPUs != .1*.000001 {
		t.Fatalf("optional inputs not normalized: %+v", s)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := l.AwaitCapacity(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled call=%v", err)
	}
	select {
	case <-p.states:
		t.Fatal("canceled call must not invoke policy")
	default:
	}
}

func TestCustomPolicyUpdateReleasesWaiter(t *testing.T) {
	p := &customGate{states: make(chan admission.State, 16)}
	l := cpulimit.New(cpulimit.Config{Window: time.Second, SampleEvery: 10 * time.Millisecond}, idleMeter{}, p)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { l.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("controller did not stop")
		}
	})
	result := make(chan error, 1)
	go func() { result <- l.AwaitCapacity(ctx, 1) }()
	select {
	case <-p.states:
	case <-time.After(time.Second):
		t.Fatal("policy was not consulted")
	}
	p.allow.Store(true) // Update the policy, without replacing it or notifying the limiter.
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("periodic retries did not observe the policy update")
	}
}
