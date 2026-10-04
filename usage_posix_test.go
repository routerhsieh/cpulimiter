//go:build linux || darwin || freebsd

package cpulimit

import "testing"

func TestPOSIXDefaultMeterReturnsMonotonicCPUTime(t *testing.T) {
	l := New(regressionConfig(), nil, nil)
	previous, err := l.meter.ProcessCPU()
	if err != nil {
		t.Fatal(err)
	}
	if previous < 0 {
		t.Fatalf("negative cumulative CPU time: %v", previous)
	}
	for i := 0; i < 10; i++ {
		current, err := l.meter.ProcessCPU()
		if err != nil {
			t.Fatal(err)
		}
		if current < previous {
			t.Fatalf("CPU time decreased from %v to %v", previous, current)
		}
		previous = current
	}
}
