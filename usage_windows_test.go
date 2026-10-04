//go:build windows

package cpulimit

import (
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// Item 9: process CPU FILETIMEs are durations in raw 100-nanosecond ticks,
// not absolute timestamps requiring a Windows-to-Unix epoch adjustment.
func TestWindowsProcessCPUUsesDurationTicks(t *testing.T) {
	h, err := windows.GetCurrentProcess()
	if err != nil {
		t.Fatal(err)
	}
	readRaw := func() time.Duration {
		t.Helper()
		var creation, exit, kernel, user windows.Filetime
		if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
			t.Fatal(err)
		}
		kernelTicks := uint64(kernel.HighDateTime)<<32 | uint64(kernel.LowDateTime)
		userTicks := uint64(user.HighDateTime)<<32 | uint64(user.LowDateTime)
		return time.Duration(kernelTicks+userTicks) * 100 * time.Nanosecond
	}
	// Bracket the meter reading with raw OS readings to allow CPU consumed by
	// these calls themselves without relying on a wall-clock tolerance.
	before := readRaw()
	got, err := (&winMeter{h: h}).ProcessCPU()
	if err != nil {
		t.Fatal(err)
	}
	after := readRaw()
	if got < before || got > after {
		t.Fatalf("ProcessCPU = %v, want cumulative duration between %v and %v", got, before, after)
	}
}
