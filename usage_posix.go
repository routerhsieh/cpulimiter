//go:build linux || darwin || freebsd

package cpulimit

import (
	"time"

	"golang.org/x/sys/unix"
)

type rusageMeter struct{}

func defaultUsageMeter() UsageMeter { return &rusageMeter{} }

func (r *rusageMeter) ProcessCPU() (time.Duration, error) {
	var ru unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &ru); err != nil {
		return 0, err
	}
	u := time.Duration(ru.Utime.Sec)*time.Second + time.Duration(ru.Utime.Usec)*time.Microsecond
	s := time.Duration(ru.Stime.Sec)*time.Second + time.Duration(ru.Stime.Usec)*time.Microsecond
	return u + s, nil
}
