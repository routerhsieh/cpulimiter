//go:build e2e && (darwin || linux)

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	cpulimit "github.com/routerhsieh/cpulimiter"
)

var (
	smoke      = flag.Bool("e2e.smoke", false, "run a small CPU matrix only")
	longRun    = flag.Bool("e2e.long", false, "also run single-core and multi-core long-window cases for both policies")
	longWindow = flag.Duration("e2e.long-window", 5*time.Minute, "long-suite window; override only for shorter harness diagnostics")
	window     = flag.Duration("e2e.window", time.Second, "short-suite averaging window (at least 500ms)")
	output     = flag.String("e2e.output", "", "directory for workload specifications and JSON measurements")
)

func config(mode cpulimit.Mode, cpus float64, w time.Duration) cpulimit.Config {
	fraction := cpus
	if mode == cpulimit.AllCores {
		fraction /= float64(runtime.NumCPU())
	}
	return cpulimit.Config{Mode: mode, GlobalFrac: fraction, Window: w,
		SampleEvery: 50 * time.Millisecond, Hysteresis: cpus * .03,
		Aging: cpulimit.AgingConfig{RampDuration: 30 * time.Second, MaxFracBoost: .1}}
}

func measuredPhase(name string, cfg cpulimit.Config, cpus float64) phase {
	return phase{Name: name, Config: cfg, TargetCPUs: cpus, Warmup: 2 * cfg.Window, Measure: 4 * cfg.Window, Duty: 1}
}

// Long workloads measure startup and all subsequent complete windows, rather
// than excluding an initial warmup. Four windows give twenty minutes by default.
func longPhase(name string, cfg cpulimit.Config, cpus float64) phase {
	return phase{Name: name, Config: cfg, TargetCPUs: cpus, Measure: 4 * cfg.Window, Duty: 1}
}

func baseWorkload(name, policy string, workers int, phases ...phase) workload {
	return workload{Name: name, Policy: policy, Workers: workers, Fractions: []float64{1}, ChunkBytes: 256 * 1024, Phases: phases}
}

func checkEnvironment(t *testing.T) {
	t.Helper()
	if *window < 500*time.Millisecond {
		t.Fatal("e2e.window must be at least 500ms; shorter windows make CPU assertions unreliable")
	}
	if *longRun && *longWindow < 500*time.Millisecond {
		t.Fatal("e2e.long-window must be at least 500ms")
	}
	if testing.Short() {
		t.Skip("real CPU workloads disabled by -short")
	}
}

func runProcess(t *testing.T, spec workload, interruptAfter time.Duration) workloadResult {
	t.Helper()
	dir := t.TempDir()
	specPath, resultPath, readyPath := filepath.Join(dir, "spec.json"), filepath.Join(dir, "result.json"), filepath.Join(dir, "ready")
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(specPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	// Persist the attempted workload before launch, including cases that fail
	// before producing their final metrics. Do not rely on t.TempDir surviving.
	attempt := ""
	if *output != "" {
		name := strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
		attempt = filepath.Join(*output, name+".attempt")
		if err := os.MkdirAll(attempt, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(attempt, "spec.json"), data, 0600); err != nil {
			t.Fatal(err)
		}
		// Let the child write directly into the durable bundle, including when
		// interruption prevents this test process from running deferred cleanup.
		resultPath = filepath.Join(attempt, "result.json")
	}
	timeout := 15 * time.Second
	for _, p := range spec.Phases {
		timeout += p.Warmup + p.Measure
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWorkloadProcess$", "-test.count=1", "-test.timeout="+timeout.String())
	cmd.Env = append(os.Environ(), "CPULIMITER_E2E_SPEC="+specPath, "CPULIMITER_E2E_RESULT="+resultPath, "CPULIMITER_E2E_READY="+readyPath)
	var log bytes.Buffer
	cmd.Stdout, cmd.Stderr = &log, &log
	if attempt != "" {
		childLog, err := os.Create(filepath.Join(attempt, "child.log"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := childLog.Close(); err != nil {
				t.Errorf("close child log: %v", err)
			}
		}()
		writer := io.MultiWriter(childLog, &log)
		cmd.Stdout, cmd.Stderr = writer, writer
	}
	started := time.Now()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	waited := false
	defer func() {
		cancel()
		if !waited {
			<-done
		}
	}()
	if interruptAfter > 0 {
		// Wait for workers/controller to exist, then let CPU history accumulate
		// before sending the same signal as Ctrl+C.
		for {
			if _, err := os.Stat(readyPath); err == nil {
				break
			}
			select {
			case err := <-done:
				waited = true
				t.Fatalf("workload exited before readiness: %v\n%s", err, log.String())
			case <-ctx.Done():
				t.Fatal("timed out waiting for workload readiness")
			case <-time.After(10 * time.Millisecond):
			}
		}
		if !pause(ctx, interruptAfter) {
			t.Fatal("timeout before interrupt")
		}
		if err := cmd.Process.Signal(os.Interrupt); err != nil {
			t.Fatal(err)
		}
	}
	cmdErr := <-done
	waited = true
	if err := cmdErr; err != nil {
		t.Fatalf("workload %s failed (timeout=%v): %v\n%s", spec.Name, ctx.Err(), err, log.String())
	}
	wall := time.Since(started)
	data, err = os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("missing workload report: %v\n%s", err, log.String())
	}
	var result workloadResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	result.ExternalWall = wall
	result.ExternalCPU = cmd.ProcessState.UserTime() + cmd.ProcessState.SystemTime()
	if *output != "" {
		if err := os.MkdirAll(*output, 0755); err != nil {
			t.Fatal(err)
		}
		artifact := struct {
			Spec   workload       `json:"workload"`
			Result workloadResult `json:"result"`
		}{spec, result}
		data, err := json.MarshalIndent(artifact, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		name := strings.NewReplacer("/", "_", " ", "_").Replace(t.Name()) + ".json"
		if err := os.WriteFile(filepath.Join(*output, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("%s: %s/%s %s logicalCPUs=%d GOMAXPROCS=%d whole-process=%.3f CPUs shutdown=%s goroutines=%d->%d",
		spec.Name, result.OS, result.Arch, result.GoVersion, result.LogicalCPUs, result.GOMAXPROCS,
		float64(result.ExternalCPU)/float64(wall), result.Shutdown, result.GoroutinesBefore, result.GoroutinesAfter)
	if result.Shutdown > 2*time.Second {
		t.Errorf("shutdown took %s", result.Shutdown)
	}
	if result.GoroutinesAfter > result.GoroutinesBefore+2 {
		t.Errorf("goroutines did not return near baseline: %d -> %d", result.GoroutinesBefore, result.GoroutinesAfter)
	}
	if interruptAfter == 0 && (result.Canceled || len(result.Phases) != len(spec.Phases)) {
		t.Fatalf("workload ended early: %+v", result)
	}
	return result
}

func chunks(p phaseResult) uint64 {
	var total uint64
	for _, w := range p.Workers {
		total += w.Chunks
	}
	return total
}

// The maximum is diagnostic: cooperative admission does not promise that every
// rolling window, much less every instantaneous sample, stays below the target.
func rollingMaximum(samples []cpuSample, window time.Duration) float64 {
	var peak float64
	left := 0
	for right := 1; right < len(samples); right++ {
		for left+1 < right && samples[right].Elapsed-samples[left+1].Elapsed >= window {
			left++
		}
		dt := samples[right].Elapsed - samples[left].Elapsed
		if dt >= window {
			peak = math.Max(peak, float64(samples[right].CPU-samples[left].CPU)/float64(dt))
		}
	}
	return peak
}

func validatePhase(t *testing.T, p phase, r phaseResult, lower float64, checkUpper bool) {
	t.Helper()
	if !r.Complete || r.Wall < p.Measure {
		t.Fatalf("incomplete phase %q: %+v", p.Name, r)
	}
	if len(r.Samples) < 3 {
		t.Fatalf("too few independent CPU samples: %d", len(r.Samples))
	}
	mean := float64(r.CPU) / float64(r.Wall)
	t.Logf("phase=%s target=%.3f mean=%.3f rolling-max=%.3f CPUs chunks=%d", p.Name, p.TargetCPUs, mean, rollingMaximum(r.Samples, p.Config.Window), chunks(r))
	for i, worker := range r.Workers {
		t.Logf("worker=%d frac=%.2f chunks=%d wait-buckets=%v cumulative-max-wait=%s", i, worker.Fraction, worker.Chunks, worker.Waits, worker.MaxWait)
	}
	if mean < lower {
		t.Errorf("phase %s: mean %.3f CPUs below minimum %.3f", p.Name, mean, lower)
	}
	// Broad bounds accommodate burstiness and OS scheduling; these are explicit
	// regression thresholds, not a precision CPU quota or an SLA.
	upper := p.TargetCPUs*1.35 + .06
	if checkUpper && mean > upper {
		t.Errorf("phase %s: mean %.3f CPUs above tolerance %.3f (target %.3f)", p.Name, mean, upper, p.TargetCPUs)
	}
	if p.Duty > 0 && !p.AllowNoProgress && chunks(r) == 0 {
		t.Errorf("phase %s made no work progress", p.Name)
	}
}

func TestE2ECPU(t *testing.T) {
	checkEnvironment(t)
	n := runtime.NumCPU()
	workers := n + 2
	baselinePhase := measuredPhase("unlimited", config(cpulimit.SingleCore, 1, *window), 0)
	baseline := baseWorkload("unlimited", "deterministic", workers, baselinePhase)
	baseline.Unlimited = true
	r := runProcess(t, baseline, 0)
	validatePhase(t, baselinePhase, r.Phases[0], .1, false)
	capacity := float64(r.Phases[0].CPU) / float64(r.Phases[0].Wall)
	type cpuCase struct {
		name           string
		mode           cpulimit.Mode
		target         float64
		workers, chunk int
		underload      bool
	}
	for _, policy := range []string{"deterministic", "power"} {
		cases := []cpuCase{
			{"single_fraction", cpulimit.SingleCore, .3, 2, 256 * 1024, false},
			{"all_multicore", cpulimit.AllCores, math.Min(2.5, float64(n)*.6), workers, 256 * 1024, false},
		}
		if !*smoke {
			cases = append(cases,
				cpuCase{"single_one_core", cpulimit.SingleCore, 1, 2, 256 * 1024, false},
				cpuCase{"all_fraction", cpulimit.AllCores, .3, 1, 256 * 1024, false},
				cpuCase{"workers_below_cores", cpulimit.AllCores, math.Min(1.5, float64(n)*.6), max(1, n/2), 256 * 1024, false},
				cpuCase{"workers_equal_cores", cpulimit.AllCores, math.Min(2.5, float64(n)*.6), n, 256 * 1024, false},
				cpuCase{"long_chunks", cpulimit.AllCores, math.Min(2.5, float64(n)*.6), workers, 4 * 1024 * 1024, false},
				cpuCase{"insufficient_work", cpulimit.AllCores, 2, 1, 256 * 1024, true})
		}
		for _, tc := range cases {
			t.Run(policy+"/"+tc.name, func(t *testing.T) {
				if tc.name == "all_multicore" && tc.target <= 1 {
					t.Skip("requires at least two logical CPUs")
				}
				if !tc.underload && capacity < tc.target*1.25 {
					t.Skipf("unlimited baseline %.2f CPUs lacks headroom for %.2f CPU target; check quotas/GOMAXPROCS/host load", capacity, tc.target)
				}
				p := measuredPhase(tc.name, config(tc.mode, tc.target, *window), tc.target)
				spec := baseWorkload(tc.name, policy, tc.workers, p)
				spec.ChunkBytes = tc.chunk
				result := runProcess(t, spec, 0)
				lower := tc.target * .35
				if tc.underload {
					lower = .1
				}
				validatePhase(t, p, result.Phases[0], lower, true)
				if tc.underload && float64(result.Phases[0].CPU)/float64(result.Phases[0].Wall) > 1.25 {
					t.Error("single worker unexpectedly used more than one core plus observer overhead")
				}
				if tc.name == "all_multicore" && tc.target >= 2 && float64(result.Phases[0].CPU)/float64(result.Phases[0].Wall) <= 1 {
					t.Error("multi-core case did not sustain more than one CPU")
				}
			})
		}
	}
}

func TestE2ERuntimeUpdates(t *testing.T) {
	checkEnvironment(t)
	if *smoke {
		t.Skip("full suite only")
	}
	for _, policy := range []string{"deterministic", "power"} {
		t.Run(policy, func(t *testing.T) {
			var phases []phase
			for _, step := range []struct {
				name   string
				mode   cpulimit.Mode
				target float64
			}{
				{"initial", cpulimit.SingleCore, .3}, {"raise_cap", cpulimit.SingleCore, .8},
				{"lower_cap", cpulimit.SingleCore, .2}, {"switch_all", cpulimit.AllCores, .6},
			} {
				cfg := config(step.mode, step.target, *window)
				if step.name == "lower_cap" {
					cfg.SampleEvery = 25 * time.Millisecond
					cfg.Window = *window * 2
				}
				phases = append(phases, measuredPhase(step.name, cfg, step.target))
			}
			spec := baseWorkload("runtime_updates", policy, 2, phases...)
			r := runProcess(t, spec, 0)
			for i, p := range phases {
				validatePhase(t, p, r.Phases[i], p.TargetCPUs*.3, true)
			}
			initial := float64(r.Phases[0].CPU) / float64(r.Phases[0].Wall)
			raised := float64(r.Phases[1].CPU) / float64(r.Phases[1].Wall)
			lowered := float64(r.Phases[2].CPU) / float64(r.Phases[2].Wall)
			if raised < initial*1.25 || raised < lowered*1.5 {
				t.Errorf("cap changes did not affect observed usage: initial=%.3f raised=%.3f lowered=%.3f", initial, raised, lowered)
			}
		})
	}
}

func TestE2EBurstAndIdle(t *testing.T) {
	checkEnvironment(t)
	if *smoke {
		t.Skip("full suite only")
	}
	for _, policy := range []string{"deterministic", "power"} {
		t.Run(policy, func(t *testing.T) {
			cfg := config(cpulimit.SingleCore, .4, *window)
			phases := []phase{measuredPhase("saturated", cfg, .4), measuredPhase("idle", cfg, .4), measuredPhase("burst", cfg, .4), measuredPhase("resume", cfg, .4)}
			phases[1].Duty, phases[2].Duty = 0, .25
			r := runProcess(t, baseWorkload("burst_idle_resume", policy, 2, phases...), 0)
			for i, p := range phases {
				lower := .08
				if p.Duty == 0 {
					lower = 0
				}
				validatePhase(t, p, r.Phases[i], lower, true)
				if p.Duty == 0 && (chunks(r.Phases[i]) != 0 || float64(r.Phases[i].CPU)/float64(r.Phases[i].Wall) > .1) {
					t.Error("idle phase did not become idle")
				}
			}
		})
	}
}

func TestE2EPriorityAndAging(t *testing.T) {
	checkEnvironment(t)
	if *smoke {
		t.Skip("full suite only")
	}
	for _, policy := range []string{"deterministic", "power"} {
		t.Run(policy+"/mixed_priority", func(t *testing.T) {
			cfg := config(cpulimit.SingleCore, .8, *window)
			spec := baseWorkload("mixed_priority", policy, 3, measuredPhase("aging_off", cfg, .8))
			spec.Fractions = []float64{.2, .5, .8}
			r := runProcess(t, spec, 0)
			validatePhase(t, spec.Phases[0], r.Phases[0], .1, true)
			// Report all groups, without asserting scheduler ordering or starvation.
			if r.Phases[0].Workers[2].Chunks == 0 {
				t.Error("high-priority worker made no progress")
			}
		})
		t.Run(policy+"/aging_recovery", func(t *testing.T) {
			cfg := config(cpulimit.SingleCore, .8, *window)
			off := measuredPhase("aging_off", cfg, .8)
			off.AllowNoProgress = true
			cfg.Aging = cpulimit.AgingConfig{Enabled: true, Delay: *window / 4, RampDuration: *window / 2, MaxFracBoost: 2}
			on := measuredPhase("aging_on", cfg, .8)
			cfg.Aging.Enabled = false
			offAgain := measuredPhase("aging_disabled", cfg, .8)
			offAgain.AllowNoProgress = true
			spec := baseWorkload("aging_recovery", policy, 1, off, on, offAgain)
			spec.Fractions, spec.Background = []float64{.05}, .35
			r := runProcess(t, spec, 0)
			for i, p := range spec.Phases {
				validatePhase(t, p, r.Phases[i], .15, true)
			}
			if chunks(r.Phases[0]) != 0 || chunks(r.Phases[2]) != 0 {
				t.Error("low-fraction probe passed while sustained background usage should exclude it")
			}
			if chunks(r.Phases[1]) == 0 {
				t.Error("aging did not restore low-fraction progress under real background load")
			}
		})
	}
}

func TestE2EInterrupt(t *testing.T) {
	checkEnvironment(t)
	if *smoke {
		t.Skip("full suite only")
	}
	for _, policy := range []string{"deterministic", "power"} {
		t.Run(policy, func(t *testing.T) {
			cfg := config(cpulimit.SingleCore, .3, *window)
			spec := baseWorkload("interrupt", policy, runtime.NumCPU()+2, measuredPhase("waiting", cfg, .3))
			r := runProcess(t, spec, *window)
			if !r.Canceled {
				t.Error("SIGINT did not cancel workload")
			}
		})
	}
}

func TestE2EDefaultWindow(t *testing.T) {
	checkEnvironment(t)
	if !*longRun || *smoke {
		t.Skip("requires -e2e.long; default-window cases take forty minutes for both policies")
	}
	logLongWindow(t)
	for _, policy := range []string{"deterministic", "power"} {
		t.Run(policy, func(t *testing.T) {
			// Explicit equivalent of the library defaults, so SetConfig equality
			// and the report include the actual settings instead of zero values.
			cfg := cpulimit.Config{GlobalFrac: .7, Window: *longWindow, SampleEvery: 100 * time.Millisecond, Hysteresis: .02,
				Aging: cpulimit.AgingConfig{RampDuration: 30 * time.Second, MaxFracBoost: .1}}
			p := longPhase("default_window", cfg, .7)
			r := runProcess(t, baseWorkload("default_window", policy, 2, p), 0)
			validateLongPhase(t, p, r.Phases[0])
		})
	}
}

func logLongWindow(t *testing.T) {
	t.Helper()
	if *longWindow != 5*time.Minute {
		t.Logf("DIAGNOSTIC: long-window override=%s; this does not validate the five-minute window", *longWindow)
	}
}

// Check the observed rolling windows as well as the entire measured phase.
// Bounds remain regression tolerances, not an instantaneous quota or exact SLA.
func validateLongPhase(t *testing.T, p phase, r phaseResult) {
	t.Helper()
	validatePhase(t, p, r, p.TargetCPUs*.35, true)
	if len(r.Samples) < 2 || r.Samples[len(r.Samples)-1].Elapsed < p.Config.Window {
		t.Fatal("long-window case did not observe a complete rolling window")
	}
	peak := rollingMaximum(r.Samples, p.Config.Window)
	upper := p.TargetCPUs*1.35 + .06
	if peak > upper {
		t.Errorf("phase %s: observed rolling mean %.3f CPUs above tolerance %.3f", p.Name, peak, upper)
	}
}

func TestE2EMulticoreLongWindow(t *testing.T) {
	checkEnvironment(t)
	if !*longRun || *smoke {
		t.Skip("requires -e2e.long; multi-core cases take two hours for both policies")
	}
	logLongWindow(t)
	n := runtime.NumCPU()
	if n < 2 {
		t.Skip("multi-core long-window coverage requires at least two logical CPUs")
	}
	// Leave calibration headroom on small hosts while still targeting >1 CPU.
	target := math.Min(2.5, float64(n)*.7)
	type multicoreCase struct {
		name           string
		workers, chunk int
	}
	cases := []multicoreCase{
		{"workers_equal_cores", n, 256 * 1024},
		{"workers_above_cores", n + 2, 256 * 1024},
		{"long_chunks", n + 2, 4 * 1024 * 1024},
	}
	for _, policy := range []string{"deterministic", "power"} {
		for _, tc := range cases {
			t.Run(policy+"/"+tc.name, func(t *testing.T) {
				// Calibrate the same worker count/chunk size immediately before
				// each long case, so narrow test filters still include calibration.
				var available float64
				if !t.Run("calibration", func(t *testing.T) {
					p := measuredPhase("unlimited", config(cpulimit.AllCores, target, *window), 0)
					spec := baseWorkload("long_window_calibration", policy, tc.workers, p)
					spec.Unlimited, spec.ChunkBytes = true, tc.chunk
					r := runProcess(t, spec, 0)
					validatePhase(t, p, r.Phases[0], .1, false)
					available = float64(r.Phases[0].CPU) / float64(r.Phases[0].Wall)
				}) {
					return
				}
				if available == 0 {
					t.Fatal("calibration was excluded by test filter")
				}
				if available < target*1.25 {
					t.Skipf("unlimited baseline %.2f CPUs lacks headroom for %.2f CPU target; multi-core case not validated", available, target)
				}
				p := longPhase(tc.name, config(cpulimit.AllCores, target, *longWindow), target)
				spec := baseWorkload("multicore_long_window_"+tc.name, policy, tc.workers, p)
				spec.ChunkBytes = tc.chunk
				r := runProcess(t, spec, 0)
				validateLongPhase(t, p, r.Phases[0])
				if float64(r.Phases[0].CPU)/float64(r.Phases[0].Wall) <= 1 {
					t.Error("multi-core long-window case did not sustain more than one CPU")
				}
			})
		}
	}
}
