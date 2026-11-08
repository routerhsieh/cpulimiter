# Unix CPU workload E2E tests

These opt-in tests run real SHA-256 workloads in separate processes on macOS and
Linux, using the public limiter API and its default OS CPU meter. They are Go
tests; no shell-specific CPU parser, `top`, `pidstat`, or PowerShell is required.
The current observer and signal tests are Unix-specific. Windows support would
need a process measurement/signal implementation, rather than necessarily a
PowerShell rewrite. Ordinary `go test ./...` does not run these workloads.

## Run

From the repository root, with Go 1.22 or later:

```sh
make test-e2e-smoke   # Unlimited baseline + two modes/policies; about 30 seconds
make test-e2e         # Full short-window suite; approximately 5 minutes
make test-e2e-long    # Also test the default five-minute window; about an hour
```

Each Make invocation creates a unique report directory under `build/e2e/`, for
example `build/e2e/20261107T214000Z-test-e2e-XXXXXX/`. The actual suffix is generated
by `mktemp` to prevent collisions even for runs started in the same second.
The timestamp uses UTC, and the directory path is printed before testing starts.
`E2E_OUTPUT` overrides the parent directory; all three E2E targets append a new
timestamped directory. Existing reports are preserved even if a run fails.
The default short window is one
second, with two windows of warmup and four windows of measurement per phase.
The suite runs sequentially to avoid competing with its own workload processes.
Do not run it concurrently with another workload suite or a benchmark.

```sh
# Faster diagnostic run (500ms is the minimum accepted short window).
make test-e2e E2E_ARGS='-e2e.window=500ms'

# Select just one group, preserving Go's test name filtering.
make test-e2e E2E_ARGS='-test.run=TestE2EPriorityAndAging'

# Without Make; absolute report paths avoid Go's package-directory working dir.
go test -tags=e2e -count=1 -v -timeout=15m ./e2e \
  -e2e.output=/tmp/cpulimiter-e2e
```

`-short` skips workloads. Do not use `-race` to assess CPU tuning: instrumentation
changes workload costs. Use the normal repository race checks separately.
With the current build tags, other operating systems have no E2E tests; a
successful command on Windows does not establish runtime coverage there.

## Coverage

| Group | Scenarios |
| --- | --- |
| `TestE2ECPU` | Unlimited calibration; both built-in policies; SingleCore and AllCores; fractional, one-core, and multi-core targets; one worker, below/equal/above logical core count; 256 KiB and 4 MiB chunks; insufficient work to fill the allowance. |
| `TestE2ERuntimeUpdates` | Raise/lower cap, change window and sampling cadence, switch SingleCore to AllCores while workers are active. |
| `TestE2EBurstAndIdle` | Saturation, idle, synchronized 100ms bursts, then sustained work again. |
| `TestE2EPriorityAndAging` | Mixed fractions 0.2/0.5/0.8; uncooperative background work excludes a 0.05-fraction probe; enable aging to restore progress; disable aging to restore exclusion. A boost of 2 exercises the effective-frac cap. Both policies are tested. |
| `TestE2EInterrupt` | SIGINT while workers/controller are running, cooperative cancellation, bounded shutdown and goroutine cleanup. |
| `TestE2EDefaultWindow` | Opt-in five-minute history, ten-minute warmup and twenty-minute measurement per policy, using default CPU settings. |

The primary multi-core target is `min(2.5, 0.6 * logical CPUs)`, with more workers
than cores. On hosts where that target is at least 2 CPUs, the case must actually
sustain more than one CPU. Cases lacking 25% headroom in the measured unlimited
baseline are explicitly skipped, rather than presented as successful multi-core
validation. Single-logical-CPU hosts skip the main multi-core case. Container
quotas, CPU affinity, GOMAXPROCS and competing host activity can limit headroom;
the report records the logical CPU count and GOMAXPROCS, not a cgroup quota.

Mixed-priority results are observational: the tests do not require every low
priority worker to progress, FIFO behavior, or a probabilistic ordering between
workers. The controlled background-load case does assert aging-driven progress.
The exact aging formula, per-call reset and allowance boundaries remain covered
by deterministic UTs; process-level CPU observations cannot prove those internal
properties independently.

## Measurements and pass criteria

CPU values are in logical cores: **1 CPU = 100% of one logical core**, irrespective
of machine size. Each phase discards its warmup before collecting measurements.

* The observer reads cumulative `getrusage(RUSAGE_SELF)` CPU time directly on a
  separate path from the limiter's history. Both naturally use the same OS source;
  the observer does not read the limiter's own average or inject a fake meter.
* The parent also records whole-process user + system CPU via `ProcessState` after
  waiting for the child, including startup, warmup and shutdown. This is an
  external accounting cross-check, distinct from the steady-phase result.
* Reports retain CPU samples for rolling-window analysis. Sample spacing is
  `Window/10`, bounded to 10–100ms. The maximum observed rolling average is logged
  for diagnosis, not asserted as a hard cap or used to claim an exact SLA.
* Worker reports include measured-phase chunks and successful admission wait
  buckets: `<1ms`, `<10ms`, `<100ms`, `<1s`, `<5s`, `<30s`, and `>=30s`. Maximum
  successful wait is cumulative across phases. Canceled waits are excluded.

For ordinary saturated cases the measured phase must make progress and average
at least 35% of the target, with an upper regression threshold of
`1.35 * target + 0.06 CPU`. Specialized phases use explicit lower bounds: idle
requires zero chunks and at most 0.1 CPU; insufficient-work cases permit low
utilization; aging-off phases permit a blocked probe while checking background
CPU is present. Runtime cap changes must also show a meaningful usage change.
Shutdown must finish within two seconds, and goroutines must return within two
of the initial baseline after a bounded cleanup period.

These deliberately broad thresholds detect gross regressions, not small tuning
differences. Failures include raw results; inspect them before changing a bound.
On heavily contended hosts, a failed utilization or background-load assumption
may indicate an unsuitable test environment. CPU limits are cooperative, so
instantaneous spikes and some short-window overshoot are expected.

Each JSON artifact contains the complete workload specification, phase samples,
worker statistics, platform/toolchain metadata and external CPU accounting. Use
`go test -json` if machine-readable test pass/fail/skip status is also needed;
skipped scenarios do not have workload artifacts. The Make targets isolate each
run in its own directory. Direct `go test` invocations do not add a timestamp:
reusing `-e2e.output` directly replaces matching report names and can leave stale
reports from other scenarios. Do not infer Linux validation
from a macOS run or from cross-compilation. This is a representative regression
matrix, not exhaustive coverage of all scheduler interleavings or workloads.

## Validation findings

### macOS

On the initial 14-logical-CPU Apple Silicon host, the 500ms-window matrix exposed
Deterministic overshoot at a 2.5 CPU target: approximately 5.17 CPUs with 16 workers,
3.67 with 14 workers, and 5.83 with 16 workers and 4 MiB chunks. Those cases fail
the stated thresholds. The separate one-second-window smoke passed, but its
Deterministic multi-core mean was still approximately 3.41 CPUs versus the 2.5 CPU
target; PowerCurve was approximately 2.13 CPUs.

The full one-second-window run also failed those three Deterministic cases:
4.48 CPUs (16 workers), 4.23 CPUs (14 workers), and 5.37 CPUs (16 workers, 4 MiB
chunks), all at the same 2.5 CPU target. Other short-suite scenarios passed,
including runtime updates, burst/idle recovery, aging recovery and SIGINT cleanup.
The full runs used Go 1.26.6 on darwin/arm64. Ordinary UTs and E2E static checks
passed, and the Linux amd64 E2E executable cross-compiled. The optional
five-minute-window runs have not been performed in this validation.

### Linux

The supplied `e2e-linux.tar.gz` contains 27 workload reports and the complete
`make test-e2e` log. The run used Linux amd64, Go 1.22.2, 12 logical CPUs and
GOMAXPROCS=12. It completed in approximately 260 seconds with the default
one-second short window, two-second warmup and four-second measured phases
(the runtime-update case also includes a two-second window). The three failures
were the same Deterministic multi-core cases observed on macOS:

| Scenario | Target, CPUs | Deterministic mean, CPUs | PowerCurve mean, CPUs |
| --- | --- | --- | --- |
| 14 workers, 256 KiB chunks | 2.50 | 5.828 (fail) | 2.081 (pass) |
| 12 workers, 256 KiB chunks | 2.50 | 5.765 (fail) | 2.074 (pass) |
| 14 workers, 4 MiB chunks | 2.50 | 5.676 (fail) | 2.249 (pass) |

The upper regression threshold for these cases is 3.435 CPUs. External
whole-process CPU accounting also shows elevated Deterministic usage, at
approximately 5.75, 5.71 and 5.45 CPUs respectively; the overshoot is not confined
to an isolated observation sample. PowerCurve's observed one-second rolling
maxima in these cases were approximately 2.10, 2.09 and 2.30 CPUs respectively.

All other executed scenarios passed for both policies, including runtime cap and
mode updates, burst/idle recovery, mixed priority, aging recovery and SIGINT
cleanup. Deterministic also passed the fractional and one-core CPU targets,
the six-worker 1.5 CPU target, and the insufficient-work case; it did not fail
every CPU-control scenario. All executed PowerCurve scenarios passed the stated
thresholds, which does not establish correctness for every workload or tighter
CPU tolerances.

In the controlled aging scenario, both policies recorded zero probe chunks with
aging disabled, ten chunks with aging enabled, and zero after disabling it again,
while background process usage stayed near 0.35 CPU. In the mixed-priority case
without aging, only the 0.8-fraction worker progressed during measurement; the
0.2- and 0.5-fraction workers completed zero chunks. Cancellation and goroutine
cleanup checks passed across the run. The five-minute-window test was skipped;
the subprocess entry-point test's skip in the parent run is expected.

### Interpretation

These observations require investigation of the limiter's behavior under high
concurrency. Reproduction on both hosts shows that the observed failure is not
limited to macOS; these reports alone do not identify its root cause. The suite
retains the failing assertions; they are not skipped as
known failures or used to widen acceptance bounds. A passing smoke run is not a
claim that the full suite or tighter CPU accuracy requirements pass. No limiter
production code is changed by this E2E addition.
