// Command hashing demonstrates cooperative CPU limiting with incremental SHA-256.
package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"runtime"
	"sync/atomic"
	"time"

	cpulimit "github.com/routerhsieh/cpulimiter"
	"github.com/routerhsieh/cpulimiter/admission"
)

type options struct {
	cpuPercent                    float64
	mode, policy                  string
	workers, chunkBytes           int
	window, sampleEvery, duration time.Duration
	alpha                         float64
	unlimited                     bool
}

func main() {
	var opts options
	flag.Float64Var(&opts.cpuPercent, "cpu-percent", 5, "CPU target as a percentage of the selected reference capacity")
	flag.StringVar(&opts.mode, "mode", "single", "CPU reference: single or all")
	flag.StringVar(&opts.policy, "policy", "deterministic", "Admission policy: deterministic or power")
	flag.IntVar(&opts.workers, "workers", 2, "Number of hashing workers")
	flag.IntVar(&opts.chunkBytes, "chunk-bytes", 256*1024, "Bytes hashed between admission checks per worker")
	flag.DurationVar(&opts.window, "window", 30*time.Second, "Approximate averaging window")
	flag.DurationVar(&opts.sampleEvery, "sample-every", 100*time.Millisecond, "Base sampling interval (divided by logical CPUs in all mode)")
	flag.DurationVar(&opts.duration, "duration", time.Minute, "Run duration; Ctrl+C stops earlier")
	flag.Float64Var(&opts.alpha, "alpha", 2, "Power-curve exponent: 1 linear, >1 more aggressive, 0<alpha<1 less aggressive")
	flag.BoolVar(&opts.unlimited, "unlimited", false, "Run without the limiter for a workload comparison")
	flag.Parse()
	if err := opts.validate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := run(opts); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func (o options) validate() error {
	if math.IsNaN(o.cpuPercent) || math.IsInf(o.cpuPercent, 0) || o.cpuPercent <= 0 || o.cpuPercent > 100 {
		return errors.New("cpu-percent must be finite and between 0 (exclusive) and 100")
	}
	if o.mode != "single" && o.mode != "all" {
		return errors.New("mode must be single or all")
	}
	if o.policy != "deterministic" && o.policy != "power" {
		return errors.New("policy must be deterministic or power")
	}
	if o.workers <= 0 || o.chunkBytes <= 0 {
		return errors.New("workers and chunk-bytes must be positive")
	}
	if o.window <= 0 || o.sampleEvery <= 0 || o.duration <= 0 {
		return errors.New("window, sample-every, and duration must be positive")
	}
	if math.IsNaN(o.alpha) || math.IsInf(o.alpha, 0) || o.alpha <= 0 {
		return errors.New("alpha must be positive and finite")
	}
	return nil
}

type workerResult struct {
	worker int
	chunks uint64
	digest [sha256.Size]byte
	err    error
}

func hashWorker(ctx context.Context, worker, chunkBytes int, limiter *cpulimit.Limiter, totalChunks *atomic.Uint64) workerResult {
	// Each worker owns its hash state and input buffer. Repeated chunks form one
	// continuous stream; independent chunk hashes are not combined afterward.
	buf := make([]byte, chunkBytes)
	for i := range buf {
		buf[i] = byte(i + worker)
	}
	h := sha256.New()
	result := workerResult{worker: worker}
	for {
		if err := ctx.Err(); err != nil {
			result.err = err
			break
		}
		if limiter != nil {
			// All workers share the cap. A fraction of 1 does not reserve one
			// full cap per worker; it uses the same process-wide admission limit.
			if err := limiter.AwaitCapacity(ctx, 1); err != nil {
				result.err = err
				break
			}
		}
		// Keep this chunk short: cancellation and throttling are cooperative.
		_, _ = h.Write(buf) // hash.Hash.Write never returns an error.
		result.chunks++
		totalChunks.Add(1)
	}
	copy(result.digest[:], h.Sum(nil))
	return result
}

func run(opts options) error {
	signalCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stopSignals()
	ctx, cancel := context.WithTimeout(signalCtx, opts.duration)
	defer cancel()

	var limiter *cpulimit.Limiter
	var controllerDone chan struct{}
	if !opts.unlimited {
		mode := cpulimit.SingleCore
		if opts.mode == "all" {
			mode = cpulimit.AllCores
		}
		var policy cpulimit.AdmissionPolicy // nil selects deterministic admission.
		if opts.policy == "power" {
			policy = admission.NewPowerCurve(admission.PowerCurveConfig{SoftRatio: .8, Alpha: opts.alpha})
		}
		limiter = cpulimit.New(cpulimit.Config{
			Mode: mode, GlobalFrac: opts.cpuPercent / 100,
			Window: opts.window, SampleEvery: opts.sampleEvery,
			// Use a margin of 5% of the global allowance for this example,
			// instead of the library's larger default margin of .02 CPU.
			Hysteresis: admissionMargin(opts),
		}, nil, policy)
		controllerDone = make(chan struct{})
		go func() { defer close(controllerDone); limiter.Run(ctx) }()
	}

	fmt.Printf("PID=%d workers=%d chunk=%d bytes duration=%s unlimited=%t\n", os.Getpid(), opts.workers, opts.chunkBytes, opts.duration, opts.unlimited)
	if limiter != nil {
		fmt.Printf("target=%.2f%% mode=%s policy=%s window=%s base-sample=%s\n", opts.cpuPercent, opts.mode, opts.policy, opts.window, opts.sampleEvery)
	}
	start := time.Now()
	var totalChunks atomic.Uint64
	results := make(chan workerResult, opts.workers)
	for i := 0; i < opts.workers; i++ {
		go func(worker int) { results <- hashWorker(ctx, worker, opts.chunkBytes, limiter, &totalChunks) }(i)
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
		case <-ticker.C:
			fmt.Printf("elapsed=%s chunks=%d hashed=%.2f MiB\n", time.Since(start).Round(time.Millisecond), totalChunks.Load(), hashedMiB(totalChunks.Load(), opts.chunkBytes))
		}
	}
	// Root cancellation reaches both the controller and every worker's wait.
	var firstError error
	for i := 0; i < opts.workers; i++ {
		r := <-results
		if r.err != nil && !errors.Is(r.err, context.Canceled) && !errors.Is(r.err, context.DeadlineExceeded) && firstError == nil {
			firstError = r.err
		}
		fmt.Printf("worker=%d chunks=%d sha256=%x\n", r.worker, r.chunks, r.digest)
	}
	if controllerDone != nil {
		<-controllerDone
	}
	fmt.Printf("finished elapsed=%s chunks=%d hashed=%.2f MiB\n", time.Since(start).Round(time.Millisecond), totalChunks.Load(), hashedMiB(totalChunks.Load(), opts.chunkBytes))
	return firstError
}

func admissionMargin(o options) float64 {
	allowance := o.cpuPercent / 100
	if o.mode == "all" {
		allowance *= float64(runtime.NumCPU())
	}
	return allowance * .05
}

func hashedMiB(chunks uint64, chunkBytes int) float64 {
	return float64(chunks) * float64(chunkBytes) / (1024 * 1024)
}
