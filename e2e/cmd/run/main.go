// Command run records Go test events and produces a self-contained E2E report.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

type testEvent struct {
	Time                          time.Time
	Action, Package, Test, Output string
	Elapsed                       float64
}

type phaseAnalysis struct {
	Name        string        `json:"name"`
	Window      time.Duration `json:"window_ns"`
	Warmup      time.Duration `json:"warmup_ns"`
	Target      float64       `json:"target_cpus"`
	Mean        float64       `json:"mean_cpus"`
	FirstWindow *float64      `json:"first_observed_window_cpus,omitempty"`
	RollingMax  *float64      `json:"max_observed_window_cpus,omitempty"`
	Complete    bool          `json:"complete"`
}

type testResult struct {
	Name        string          `json:"name"`
	Status      string          `json:"status"`
	Elapsed     float64         `json:"elapsed_seconds"`
	Diagnostics []string        `json:"diagnostics,omitempty"`
	Report      string          `json:"report,omitempty"`
	Attempt     string          `json:"attempt,omitempty"`
	Policy      string          `json:"policy,omitempty"`
	Unlimited   bool            `json:"unlimited,omitempty"`
	Workers     int             `json:"workers,omitempty"`
	Chunk       int             `json:"chunk_bytes,omitempty"`
	Phases      []phaseAnalysis `json:"phases,omitempty"`
}

type summary struct {
	Version        int               `json:"schema_version"`
	Status         string            `json:"status"`
	Started        time.Time         `json:"started_at"`
	Finished       time.Time         `json:"finished_at,omitempty"`
	Command        []string          `json:"command"`
	Directory      string            `json:"working_directory"`
	OS             string            `json:"os"`
	Arch           string            `json:"arch"`
	GoVersion      string            `json:"go_version"`
	LogicalCPUs    int               `json:"logical_cpus"`
	Environment    map[string]string `json:"environment"`
	Build          map[string]string `json:"build"`
	ExitCode       int               `json:"exit_code"`
	Packages       map[string]string `json:"packages"`
	Tests          []*testResult     `json:"tests"`
	Counts         map[string]int    `json:"counts"`
	Reports        int               `json:"workload_reports"`
	LimitedReports int               `json:"limited_workload_reports"`
	Diagnostics    []string          `json:"command_diagnostics,omitempty"`
	Errors         []string          `json:"errors,omitempty"`
	Warnings       []string          `json:"warnings,omitempty"`
}

type collector struct {
	s      summary
	byName map[string]*testResult
}

func (c *collector) accept(e testEvent) {
	if e.Test == "" {
		if e.Output != "" {
			c.s.Diagnostics = append(c.s.Diagnostics, e.Output)
		}
		if e.Action == "pass" || e.Action == "fail" || e.Action == "skip" {
			c.s.Packages[e.Package] = strings.ToUpper(e.Action)
		}
		return
	}
	r := c.byName[e.Test]
	if r == nil {
		r = &testResult{Name: e.Test, Status: "RUNNING"}
		c.byName[e.Test] = r
		c.s.Tests = append(c.s.Tests, r)
	}
	switch e.Action {
	case "output":
		for _, line := range strings.Split(strings.TrimSuffix(e.Output, "\n"), "\n") {
			if line != "" && !strings.HasPrefix(strings.TrimSpace(line), "=== ") && !strings.HasPrefix(strings.TrimSpace(line), "--- ") {
				r.Diagnostics = append(r.Diagnostics, line)
			}
		}
	case "pass", "fail", "skip":
		r.Status = strings.ToUpper(e.Action)
		r.Elapsed = e.Elapsed
	}
}

func reportName(test string) string {
	return strings.NewReplacer("/", "_", " ", "_").Replace(test)
}

func (c *collector) attachReports(dir string) {
	c.s.Reports = 0
	c.s.LimitedReports = 0
	for _, r := range c.s.Tests {
		base := reportName(r.Name)
		if info, err := os.Stat(filepath.Join(dir, base+".attempt")); err == nil && info.IsDir() {
			r.Attempt = base + ".attempt"
		}
		data, err := os.ReadFile(filepath.Join(dir, base+".json"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			c.s.Errors = append(c.s.Errors, err.Error())
			continue
		}
		r.Report = base + ".json"
		var a struct {
			Workload struct {
				Policy              string
				Unlimited           bool
				Workers, ChunkBytes int
				Phases              []struct {
					Name   string
					Config struct{ Window time.Duration }
					Warmup time.Duration
				}
			}
			Result struct {
				Phases []struct {
					Name     string        `json:"name"`
					Target   float64       `json:"target_cpus"`
					Wall     time.Duration `json:"measured_wall_ns"`
					CPU      time.Duration `json:"measured_cpu_ns"`
					Complete bool          `json:"complete"`
					Samples  []struct {
						Elapsed time.Duration `json:"elapsed_ns"`
						CPU     time.Duration `json:"cpu_ns"`
					} `json:"samples"`
				}
			}
		}
		if err := json.Unmarshal(data, &a); err != nil {
			c.s.Errors = append(c.s.Errors, fmt.Sprintf("%s: %v", r.Report, err))
			continue
		}
		c.s.Reports++
		if !a.Workload.Unlimited {
			c.s.LimitedReports++
		}
		r.Policy, r.Unlimited, r.Workers, r.Chunk = a.Workload.Policy, a.Workload.Unlimited, a.Workload.Workers, a.Workload.ChunkBytes
		for i, p := range a.Result.Phases {
			analysis := phaseAnalysis{Name: p.Name, Target: p.Target, Complete: p.Complete}
			if p.Wall > 0 {
				analysis.Mean = float64(p.CPU) / float64(p.Wall)
			}
			if i < len(a.Workload.Phases) {
				analysis.Window = a.Workload.Phases[i].Config.Window
				analysis.Warmup = a.Workload.Phases[i].Warmup
			}
			left := 0
			for right := 1; right < len(p.Samples) && analysis.Window > 0; right++ {
				for left+1 < right && p.Samples[right].Elapsed-p.Samples[left+1].Elapsed >= analysis.Window {
					left++
				}
				dt := p.Samples[right].Elapsed - p.Samples[left].Elapsed
				if dt >= analysis.Window {
					rate := float64(p.Samples[right].CPU-p.Samples[left].CPU) / float64(dt)
					if analysis.FirstWindow == nil {
						v := rate
						analysis.FirstWindow = &v
					}
					if analysis.RollingMax == nil || rate > *analysis.RollingMax {
						v := rate
						analysis.RollingMax = &v
					}
				}
			}
			r.Phases = append(r.Phases, analysis)
		}
	}
}

// Save snapshots atomically so interruption cannot turn a valid summary into
// truncated JSON. A RUNNING snapshot is never presented as a completed pass.
func saveJSON(dir string, s summary) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "summary.json.tmp")
	if err = os.WriteFile(tmp, append(data, '\n'), 0600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "summary.json"))
}

func saveMarkdown(dir string, s summary) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# E2E %s\n\nStarted: %s\n\nExit code: %d. Workload reports: %d (%d limited). Test entries, including groups: PASS=%d FAIL=%d SKIP=%d INCOMPLETE=%d.\n\n", s.Status, s.Started.Format(time.RFC3339), s.ExitCode, s.Reports, s.LimitedReports, s.Counts["PASS"], s.Counts["FAIL"], s.Counts["SKIP"], s.Counts["INCOMPLETE"])
	b.WriteString("Files: [full log](run.log), [raw Go events](events.jsonl), [JSON summary](summary.json), [bootstrap log](bootstrap.log).\n\n")
	b.WriteString("| Test (includes groups) | Status | Seconds | Artifacts |\n| --- | --- | ---: | --- |\n")
	for _, r := range s.Tests {
		artifacts := ""
		if r.Report != "" {
			artifacts = fmt.Sprintf("[metrics](%s)", r.Report)
		}
		if r.Attempt != "" {
			artifacts += fmt.Sprintf(" [attempt](%s/)", r.Attempt)
		}
		fmt.Fprintf(&b, "| %s | %s | %.2f | %s |\n", strings.ReplaceAll(r.Name, "|", "\\|"), r.Status, r.Elapsed, artifacts)
	}
	for _, r := range s.Tests {
		if r.Status == "PASS" {
			continue
		}
		fmt.Fprintf(&b, "\n## %s: %s\n\n", r.Status, r.Name)
		if len(r.Diagnostics) == 0 {
			b.WriteString("No direct diagnostic; inspect child tests and run.log.\n")
		}
		for _, line := range r.Diagnostics {
			fmt.Fprintf(&b, "    %s\n", line)
		}
	}
	b.WriteString("\n## CPU measurements\n\nWindow/warmup are recorded explicitly; shortened windows are diagnostics, not five-minute validation. Rolling maxima describe sampled windows, not an exact SLA.\n\n")
	b.WriteString("| Test / phase | Policy | Window | Warmup | Complete | Target CPU | Mean CPU | First window | Max window |\n| --- | --- | --- | --- | --- | ---: | ---: | ---: | ---: |\n")
	for _, r := range s.Tests {
		for _, p := range r.Phases {
			first, peak := "n/a", "n/a"
			if p.FirstWindow != nil {
				first = fmt.Sprintf("%.4f", *p.FirstWindow)
			}
			if p.RollingMax != nil {
				peak = fmt.Sprintf("%.4f", *p.RollingMax)
			}
			policy := r.Policy
			if r.Unlimited {
				policy = "unlimited"
			}
			fmt.Fprintf(&b, "| %s / %s | %s | %s | %s | %t | %.4f | %.4f | %s | %s |\n", r.Name, p.Name, policy, p.Window, p.Warmup, p.Complete, p.Target, p.Mean, first, peak)
		}
	}
	if len(s.Diagnostics) > 0 {
		b.WriteString("\n## Command diagnostics\n\n")
		for _, text := range s.Diagnostics {
			for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
				fmt.Fprintf(&b, "    %s\n", line)
			}
		}
	}
	for _, err := range s.Errors {
		fmt.Fprintf(&b, "\nError: %s\n", err)
	}
	for _, warning := range s.Warnings {
		fmt.Fprintf(&b, "\nNote: %s\n", warning)
	}
	return os.WriteFile(filepath.Join(dir, "summary.md"), []byte(b.String()), 0600)
}

type lockedWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writer.Write(p)
}

func run(ctx context.Context, goTool, dir, timeout string, extra []string, console io.Writer) int {
	log, err := os.Create(filepath.Join(dir, "run.log"))
	if err != nil {
		fmt.Fprintln(console, err)
		return 1
	}
	defer log.Close()
	out := &lockedWriter{writer: io.MultiWriter(log, console)}
	raw, err := os.Create(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		fmt.Fprintln(out, err)
		return 1
	}
	defer raw.Close()
	cwd, _ := os.Getwd()
	args := []string{"test", "-json", "-tags=e2e", "-count=1", "-v", "-timeout=" + timeout, "./e2e"}
	args = append(args, extra...)
	// Keep all workload artifacts in this run, even if extra flags specify output.
	args = append(args, "-e2e.output="+dir)
	c := collector{
		s: summary{Version: 1, Status: "RUNNING", Started: time.Now().UTC(),
			Command: append([]string{goTool}, args...), Directory: cwd,
			OS: runtime.GOOS, Arch: runtime.GOARCH, GoVersion: runtime.Version(), LogicalCPUs: runtime.NumCPU(),
			Environment: map[string]string{}, Build: map[string]string{}, Packages: map[string]string{},
			Tests: []*testResult{}, Counts: map[string]int{}},
		byName: map[string]*testResult{},
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if strings.HasPrefix(setting.Key, "vcs") {
				c.s.Build[setting.Key] = setting.Value
			}
		}
	}
	for _, key := range []string{"GOMAXPROCS", "GODEBUG", "GOMEMLIMIT", "CGO_ENABLED", "GOFLAGS", "GOTOOLCHAIN", "GOOS", "GOARCH"} {
		if v, ok := os.LookupEnv(key); ok {
			c.s.Environment[key] = v
		}
	}
	fmt.Fprintf(out, "E2E command: %q\nReports: %s\n", c.s.Command, dir)
	finish := func(code int) int {
		c.s.Finished = time.Now().UTC()
		c.s.ExitCode = code
		for _, r := range c.s.Tests {
			if r.Status == "RUNNING" {
				r.Status = "INCOMPLETE"
				r.Diagnostics = append(r.Diagnostics, "test did not emit a terminal result")
			}
			c.s.Counts[r.Status]++
		}
		c.attachReports(dir)
		if len(c.s.Tests) == 0 && code == 0 {
			c.s.Errors = append(c.s.Errors, "no tests ran; check platform and test filter")
		}
		if c.s.LimitedReports == 0 {
			c.s.Warnings = append(c.s.Warnings, "No limited-workload reports were produced; calibration or utility tests alone do not establish CPU limiting coverage.")
		}
		if ctx.Err() != nil {
			c.s.Status = "INTERRUPTED"
			code = 130
		} else if code != 0 || c.s.Counts["FAIL"] > 0 || c.s.Counts["INCOMPLETE"] > 0 {
			c.s.Status = "FAIL"
			if code == 0 {
				code = 1
			}
		} else {
			c.s.Status = "PASS"
			if c.s.Counts["PASS"] == 0 {
				c.s.Status = "SKIP"
			}
		}
		for _, status := range c.s.Packages {
			if status == "FAIL" && code == 0 {
				code = 1
				c.s.Status = "FAIL"
			}
		}
		if len(c.s.Errors) > 0 {
			if code == 0 {
				code = 1
			}
			if c.s.Status != "INTERRUPTED" {
				c.s.Status = "ERROR"
			}
		}
		c.s.ExitCode = code
		for _, message := range c.s.Errors {
			fmt.Fprintln(out, "E2E runner error:", message)
		}
		if err := saveMarkdown(dir, c.s); err != nil {
			c.s.Errors = append(c.s.Errors, "write Markdown summary: "+err.Error())
			c.s.Status = "ERROR"
			code = 1
			c.s.ExitCode = code
			fmt.Fprintln(out, "summary error:", err)
		}
		if err := saveJSON(dir, c.s); err != nil {
			c.s.Errors = append(c.s.Errors, "write JSON summary: "+err.Error())
			c.s.Status = "ERROR"
			code = 1
			c.s.ExitCode = code
			fmt.Fprintln(out, "summary error:", err)
			_ = saveMarkdown(dir, c.s)
		}
		fmt.Fprintf(out, "\nE2E %s: test entries=%d pass=%d fail=%d skip=%d incomplete=%d reports=%d\nSummary: %s\n", c.s.Status, len(c.s.Tests), c.s.Counts["PASS"], c.s.Counts["FAIL"], c.s.Counts["SKIP"], c.s.Counts["INCOMPLETE"], c.s.Reports, filepath.Join(dir, "summary.md"))
		return code
	}
	if err := saveJSON(dir, c.s); err != nil {
		c.s.Errors = append(c.s.Errors, err.Error())
		return finish(1)
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		c.s.Errors = append(c.s.Errors, "E2E workloads currently support only macOS and Linux")
		return finish(1)
	}
	cmd := exec.Command(goTool, args...)
	configureProcess(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		c.s.Errors = append(c.s.Errors, err.Error())
		return finish(1)
	}
	var stderr strings.Builder
	cmd.Stderr = io.MultiWriter(out, &stderr)
	if err := cmd.Start(); err != nil {
		c.s.Errors = append(c.s.Errors, err.Error())
		return finish(1)
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-done:
			return
		case <-ctx.Done():
		}
		select {
		case <-done:
			return
		default:
		}
		interruptProcess(cmd)
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
			killProcess(cmd)
		}
	}()
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		var e testEvent
		if err := json.Unmarshal(line, &e); err != nil {
			fmt.Fprintln(out, string(line))
			c.s.Diagnostics = append(c.s.Diagnostics, string(line))
			continue
		}
		if _, err := raw.Write(append(append([]byte{}, line...), '\n')); err != nil {
			c.s.Errors = append(c.s.Errors, err.Error())
		}
		if e.Output != "" {
			fmt.Fprint(out, e.Output)
		}
		c.accept(e)
		if e.Action == "run" || e.Action == "pass" || e.Action == "fail" || e.Action == "skip" {
			if err := saveJSON(dir, c.s); err != nil {
				c.s.Errors = append(c.s.Errors, err.Error())
			}
		}
	}
	if err := scanner.Err(); err != nil {
		c.s.Errors = append(c.s.Errors, err.Error())
		killProcess(cmd)
	}
	err = cmd.Wait()
	close(done)
	if stderr.Len() > 0 {
		c.s.Diagnostics = append(c.s.Diagnostics, stderr.String())
	}
	code := 0
	if err != nil {
		code = 1
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else {
			c.s.Errors = append(c.s.Errors, err.Error())
		}
	}
	return finish(code)
}

func main() {
	goTool := flag.String("go", "go", "Go executable")
	dir := flag.String("output", "", "existing run directory")
	timeout := flag.String("timeout", "15m", "Go test timeout")
	archiveOnly := flag.Bool("archive-only", false, "package an existing report directory as ZIP without running tests")
	flag.Parse()
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "-output is required")
		os.Exit(2)
	}
	abs, err := filepath.Abs(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if *archiveOnly {
		archive, err := archiveReport(abs)
		if err != nil {
			_ = os.WriteFile(filepath.Join(abs, "archive-error.log"), []byte(err.Error()+"\n"), 0600)
			fmt.Fprintln(os.Stderr, "E2E ZIP archive failed:", err)
			os.Exit(1)
		}
		fmt.Println("E2E archive:", archive)
		return
	}
	if err := os.MkdirAll(abs, 0755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), terminationSignals()...)
	code := run(ctx, *goTool, abs, *timeout, flag.Args(), os.Stdout)
	stop()
	os.Exit(code)
}
