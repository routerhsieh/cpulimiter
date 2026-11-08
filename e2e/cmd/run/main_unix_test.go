//go:build darwin || linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeGo(t *testing.T, events []testEvent, exit int, stderr string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-go")
	var script strings.Builder
	script.WriteString("#!/bin/sh\ncat <<'EVENTS'\n")
	for _, e := range events {
		data, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		script.Write(data)
		script.WriteByte('\n')
	}
	script.WriteString("EVENTS\n")
	if stderr != "" {
		fmt.Fprintf(&script, "cat >&2 <<'ERRORS'\n%s\nERRORS\n", stderr)
	}
	fmt.Fprintf(&script, "exit %d\n", exit)
	if err := os.WriteFile(path, []byte(script.String()), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func readSummary(t *testing.T, dir string) summary {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s summary
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"summary.md", "run.log", "events.jsonl"} {
		if _, err := os.Stat(filepath.Join(dir, file)); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func TestRunnerRecordsPassFailSkipAndBuildErrors(t *testing.T) {
	for _, tc := range []struct {
		name           string
		events         []testEvent
		code           int
		stderr, status string
	}{
		{"pass", []testEvent{{Action: "run", Test: "TestPass"}, {Action: "pass", Test: "TestPass", Elapsed: 1}, {Action: "pass", Package: "example/e2e"}}, 0, "", "PASS"},
		{"fail", []testEvent{{Action: "run", Test: "TestFail"}, {Action: "output", Test: "TestFail", Output: "    suite_test.go:10: mean CPU above tolerance\n"}, {Action: "fail", Test: "TestFail"}, {Action: "fail", Package: "example/e2e"}}, 1, "", "FAIL"},
		{"fail_event_exit_zero", []testEvent{{Action: "run", Test: "TestFail"}, {Action: "fail", Test: "TestFail"}, {Action: "pass", Package: "example/e2e"}}, 0, "", "FAIL"},
		{"skip", []testEvent{{Action: "run", Test: "TestSkip"}, {Action: "output", Test: "TestSkip", Output: "    suite_test.go:10: insufficient CPU headroom\n"}, {Action: "skip", Test: "TestSkip"}, {Action: "pass", Package: "example/e2e"}}, 0, "", "SKIP"},
		{"build_failure", []testEvent{{Action: "fail", Package: "example/e2e"}}, 1, "compiler: undefined identifier", "FAIL"},
		{"empty_selection", []testEvent{{Action: "pass", Package: "example/e2e"}}, 0, "", "ERROR"},
		{"unfinished", []testEvent{{Action: "run", Test: "TestTimeout"}}, 1, "panic: test timed out", "FAIL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			code := run(context.Background(), fakeGo(t, tc.events, tc.code, tc.stderr), dir, "1m", nil, io.Discard)
			s := readSummary(t, dir)
			if s.Status != tc.status {
				t.Fatalf("status=%s want %s", s.Status, tc.status)
			}
			if (code != 0) != (tc.code != 0 || tc.status == "ERROR" || tc.status == "FAIL") {
				t.Fatalf("wrong exit code: %d", code)
			}
			if tc.name == "fail" && !strings.Contains(strings.Join(s.Tests[0].Diagnostics, "\n"), "above tolerance") {
				t.Fatal("failure reason missing")
			}
			if tc.name == "skip" && !strings.Contains(strings.Join(s.Tests[0].Diagnostics, "\n"), "headroom") {
				t.Fatal("skip reason missing")
			}
			if tc.stderr != "" && !strings.Contains(strings.Join(s.Diagnostics, "\n"), tc.stderr) {
				t.Fatal("command stderr missing")
			}
			if tc.name == "unfinished" && s.Tests[0].Status != "INCOMPLETE" {
				t.Fatal("unfinished test presented as complete")
			}
		})
	}
}

func TestRunnerLinksArtifactsAndAnalyzesFirstWindow(t *testing.T) {
	dir := t.TempDir()
	data := `{"workload":{"Policy":"deterministic","Workers":16,"ChunkBytes":262144,"Phases":[{"Config":{"Window":1000000000},"Warmup":0}]},"result":{"phases":[{"name":"startup","target_cpus":2.5,"measured_wall_ns":4000000000,"measured_cpu_ns":6000000000,"complete":true,"samples":[{"elapsed_ns":0,"cpu_ns":0},{"elapsed_ns":1000000000,"cpu_ns":0},{"elapsed_ns":2000000000,"cpu_ns":6000000000},{"elapsed_ns":4000000000,"cpu_ns":6000000000}]}]}}`
	if err := os.WriteFile(filepath.Join(dir, "TestCPU_case.json"), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "TestCPU_case.attempt"), 0755); err != nil {
		t.Fatal(err)
	}
	tool := fakeGo(t, []testEvent{{Action: "run", Test: "TestCPU/case"}, {Action: "fail", Test: "TestCPU/case"}, {Action: "fail", Package: "e2e"}}, 1, "")
	if code := run(context.Background(), tool, dir, "1m", nil, io.Discard); code == 0 {
		t.Fatal("failure was masked")
	}
	s := readSummary(t, dir)
	r := s.Tests[0]
	if s.Reports != 1 || r.Report != "TestCPU_case.json" || r.Attempt != "TestCPU_case.attempt" {
		t.Fatalf("missing artifact links: %+v", r)
	}
	p := r.Phases[0]
	if p.Mean != 1.5 || p.FirstWindow == nil || *p.FirstWindow != 0 || p.RollingMax == nil || *p.RollingMax != 6 || p.Warmup != 0 {
		t.Fatalf("wrong window analysis: %+v", p)
	}
}

func TestRunnerInterruptSavesIncompleteResult(t *testing.T) {
	dir := t.TempDir()
	tool := filepath.Join(t.TempDir(), "waiting-go")
	script := "#!/bin/sh\ntrap 'exit 130' INT TERM\nprintf '%s\\n' '{\"Action\":\"run\",\"Test\":\"TestWaiting\"}'\nsleep 30\n"
	if err := os.WriteFile(tool, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- run(ctx, tool, dir, "1m", nil, io.Discard) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		data, _ := os.ReadFile(filepath.Join(dir, "summary.json"))
		var s summary
		if json.Unmarshal(data, &s) == nil && len(s.Tests) == 1 {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("runner did not record test start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case code := <-done:
		if code != 130 {
			t.Fatalf("interrupt exit=%d", code)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("interrupt did not stop process group")
	}
	s := readSummary(t, dir)
	if s.Status != "INTERRUPTED" || s.Tests[0].Status != "INCOMPLETE" {
		t.Fatalf("incorrect interrupted status: %+v", s)
	}
}
