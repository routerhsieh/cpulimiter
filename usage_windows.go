//go:build windows

package cpulimit

import (
	"time"

	"golang.org/x/sys/windows"
)

type winMeter struct{ h windows.Handle }

func defaultUsageMeter() UsageMeter {
	h, _ := windows.GetCurrentProcess()
	return &winMeter{h: h}
}

func (w *winMeter) ProcessCPU() (time.Duration, error) {
	var c, e, k, u windows.Filetime
	if err := windows.GetProcessTimes(w.h, &c, &e, &k, &u); err != nil {
		return 0, err
	}
	// Kernel/user FILETIMEs are durations, not timestamps. Convert raw
	// 100-nanosecond ticks without applying an epoch adjustment.
	kernelTicks := uint64(k.HighDateTime)<<32 | uint64(k.LowDateTime)
	userTicks := uint64(u.HighDateTime)<<32 | uint64(u.LowDateTime)
	return time.Duration(kernelTicks+userTicks) * 100 * time.Nanosecond, nil
}
