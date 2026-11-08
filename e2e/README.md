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
make test-e2e-long    # Also test five-minute single/multi-core windows; about 2h 45m
```

Each Make invocation creates a unique report directory under `build/e2e/`, for
example `build/e2e/20261107T214000Z-test-e2e-XXXXXX/`. The actual suffix is generated
by `mktemp` to prevent collisions even for runs started in the same second.
The timestamp uses UTC, and the directory path is printed before testing starts.
`E2E_OUTPUT` overrides the parent directory; all three E2E targets append a new
timestamped directory. Existing reports are preserved even if a run fails.
At exit, the Go report tool creates a matching `.zip` beside that directory,
for example `build/e2e/20261107T214000Z-test-e2e-XXXXXX.zip`, and prints its path.
Download that one archive to share the complete run. Packing uses Go's standard
`archive/zip` package; no external `tar`, `gzip` or `zip` executable is required.
The original directory is kept, and the temporary runner executable is excluded
from the ZIP. Test failures and handled interruptions still attempt packaging
and keep their original nonzero exit status. ZIP creation itself is portable,
including Windows; the workload runner still supports only macOS/Linux.

If packing fails, Make fails and keeps the original reports; `archive-error.log`
records the error when writable. Temporary ZIP files are removed, and the final
ZIP appears only after all entries are written. If the Go report tool cannot even
build or start, bootstrap logs and an ERROR summary remain in the directory, but
ZIP creation is unavailable. No external archive command is used as a fallback.
No `tee` is needed: live console output and all diagnostic artifacts are captured
automatically in that same directory. Start with `summary.md` for analysis or
`summary.json` for machine-readable results:

```text
summary.md                 PASS/FAIL/SKIP, reasons, report links and CPU/window analysis
summary.json               Statuses, diagnostics, exact command, platform and build metadata
run.log                    Full test output, compiler errors and runner messages
events.jsonl               Raw structured Go test events
bootstrap.log              Report-runner build output
Test*.json                 Original workload specifications and CPU/worker measurements
Test*.attempt/spec.json    Workload saved before the subprocess starts
Test*.attempt/child.log    Subprocess output, including crash/timeout diagnostics
Test*.attempt/result.json  Raw subprocess result, if it produced one
```

The summary's verdict comes from Go's test events after validation; the existence
of a metrics file alone does not imply PASS. Failed and skipped tests keep their
diagnostics even when no metrics exist. Compiler/setup failures retain command
diagnostics, and a failed test or report-generation error makes `make` fail.
The temporary report-runner executable is removed automatically after the run.

SIGINT/SIGTERM requests shutdown of the test process group and writes an
`INTERRUPTED` summary; tests without a terminal event become `INCOMPLETE`.
Snapshots are saved during execution. If forcibly killed (SIGKILL), a final
summary cannot be guaranteed: `BOOTSTRAPPING`/`RUNNING` indicates an unfinished
run, not success. Keep the **entire run directory** when sharing results.
Packaging on interruption is best effort; SIGKILL cannot run cleanup or guarantee
an archive. When available, share the automatically generated `.zip`.

The default short window is one
second, with two windows of warmup and four windows of measurement per phase.
Long workloads have no warmup: measurement begins before the controller and
workers are released, so startup and the first full window are included.
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

# Only long-window groups, including both policies and all worker/chunk cases.
make test-e2e-long E2E_ARGS='-test.run=Window'

# Only the six new multi-core cases, if SingleCore is already validated (~2h).
make test-e2e-long E2E_ARGS='-test.run=TestE2EMulticoreLongWindow'

# Harness diagnostic only: exercise all long-case configurations in ~2 minutes.
# This override does NOT validate five-minute behavior.
make test-e2e-long E2E_ARGS='-test.run=Window -e2e.long-window=2s'
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
| `TestE2EDefaultWindow` | Both policies, SingleCore 0.7 CPU target, two workers and 256 KiB chunks, using the default five-minute window and CPU settings. |
| `TestE2EMulticoreLongWindow` | Both policies, AllCores target above one CPU; workers equal to cores, workers above cores, and above-core workers with 4 MiB chunks. Separate unlimited calibration immediately before each case. |

The long suite has **eight limited workloads**: four configurations per policy.
Each uses a five-minute window and measures twenty minutes from startup, with
**zero warmup**. Both the first complete five-minute window and subsequent
observed rolling windows are checked.
The three multi-core configurations use `min(2.5, 0.7 * logical CPUs)` as the target;
workers are respectively `N`, `N+2` and `N+2`, with 256 KiB, 256 KiB and 4 MiB
chunks. The target stays below host capacity while allowing meaningful multi-core
usage on smaller hosts. The measured phase must actually average more than one CPU.

The six multi-core cases each get a fresh unlimited baseline with matching worker
count and chunk size, using the short-suite timing. Insufficient baseline headroom
or a single-logical-CPU host produces an explicit skip, which is missing coverage,
not successful multi-core validation. Filtering to one policy/case still runs its
nested calibration. On a suitable host the complete long target takes about two
hours and forty-five minutes, including the short suite, with a 3h30m timeout.

`-e2e.long-window` defaults to five minutes. A smaller override scales the four-window
measurement duration (warmup stays zero), prints a `DIAGNOSTIC` message, and appears
in report configuration;
it is for checking the harness, not certifying long-window behavior. Long-suite
runtime/aging/burst/cancellation scenarios still use the separate short-window
groups; these eight long cases cover sustained CPU control.

The short suite's primary multi-core target is `min(2.5, 0.6 * logical CPUs)`, with
more workers than cores. On hosts where that target is at least 2 CPUs, the case must actually
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
of machine size. Short phases discard their explicit warmup; long phases start
CPU/time observation before releasing the controller and workers, retaining
startup work. No ten-minute period is excluded from long-window acceptance.

* The observer reads cumulative `getrusage(RUSAGE_SELF)` CPU time directly on a
  separate path from the limiter's history. Both naturally use the same OS source;
  the observer does not read the limiter's own average or inject a fake meter.
* The parent also records whole-process user + system CPU via `ProcessState` after
  waiting for the child, including startup, warmup and shutdown. This is an
  external accounting cross-check, distinct from the steady-phase result.
* Reports retain CPU samples for rolling-window analysis. Sample spacing is
  `Window/10`, bounded to 10–100ms. Short groups log rolling maxima for diagnosis.
  Both long-window groups also require their observed rolling averages, starting
  with the first complete window after startup, to remain
  within the same regression tolerance used for the whole-phase mean; this is
  not an instantaneous cap or a proof about every continuously sliding window.
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

Each workload JSON contains the complete specification, phase samples,
worker statistics, platform/toolchain metadata and external CPU accounting.
The Make runner adds test verdicts and diagnostic analysis in `summary.json` and
`summary.md`; counts include test groups as well as individual tests. Skipped
scenarios have summary entries even though they have no workload measurements.
The runner also warns if only calibration or utility tests ran without any
limited-workload reports. A successful run with skips is not complete coverage.
The Make targets isolate each
run in its own directory. Direct `go test` invocations do not add a timestamp:
reusing `-e2e.output` directly replaces matching report names and can leave stale
reports from other scenarios. Do not infer Linux validation
from a macOS run or from cross-compilation. This is a representative regression
matrix, not exhaustive coverage of all scheduler interleavings or workloads.
Direct `go test` also bypasses the automatic run summary and console-log capture;
use the Make targets for a complete diagnostic bundle.

## Validation findings

The macOS and Linux findings immediately below describe the earlier count-based
sampling implementation. See the sampling correction follow-up for the current
implementation's validation results.

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

### Sampling correction follow-up

The limiter now timestamps actual CPU reads and averages CPU/wall intervals by
elapsed time. It prorates the oldest interval at the real-time window boundary;
changing cadence no longer changes historical weights or the retained duration.
Both admission policies, the core-dependent cadence and all E2E thresholds are
unchanged. The old `MovingAvg` interface and count-based `RingAvg` implementation
have been removed; `movingavg` now provides only the elapsed-time `TimeWindow`.

On the same 14-logical-CPU macOS arm64 host with Go 1.26.6, the full one-second
short-window suite passed after this correction (approximately 261 seconds).
The formerly failing Deterministic scenarios improved as follows:

| Scenario, 2.5 CPU target | Earlier mean, CPUs | Corrected mean, CPUs |
| --- | --- | --- |
| 16 workers, 256 KiB chunks | 4.48 | 2.579 |
| 14 workers, 256 KiB chunks | 4.23 | 2.637 |
| 16 workers, 4 MiB chunks | 5.37 | 2.508 |

PowerCurve passed as well, with corresponding means of 2.141, 2.130 and 2.265 CPUs.
Runtime updates, burst/idle recovery, mixed priority, aging recovery and SIGINT
cleanup also passed. UT and race checks cover unequal intervals, missing reads,
counter resets, window resizing, fractional boundaries and delayed meter reads.

The three Deterministic cases were also rerun with the original 500ms window and
passed unchanged mean-usage thresholds: approximately 3.02, 2.85 and 3.09 CPUs
respectively. They still overshoot the 2.5 CPU target, and observed rolling maxima
can exceed the mean-usage threshold; the correction is not a strict quota.

This verifies the correction on the tested Mac workload, not a hard CPU ceiling.
Boundary prorating assumes uniform usage within an observation interval; finer
CPU timing is unavailable during missing reads. Linux execution of the corrected
code still needs validation.

### First five-minute-window run and expanded coverage

The supplied macOS long-run log `e2e-long-20261108-001218.log` completed the original
short suite and two SingleCore long cases in approximately 64 minutes 21 seconds,
with all workloads passing. With two workers and a 0.7 CPU target, the measured
twenty-minute means were 0.68008 CPU for Deterministic and 0.56423 CPU for PowerCurve.
The observed five-minute rolling maxima were 0.68072 and 0.56525 CPU respectively.

That run did **not** cover multi-core five-minute windows. The long suite has now
been expanded to the eight limited workloads described above, with separate
calibration and observed rolling-window assertions. The six new multi-core
five-minute cases have not yet been run at their full duration. Shortened
`-e2e.long-window` diagnostics do not change that validation status.

Before removing warmup, the expanded matrix was exercised on the Mac with
`-e2e.long-window=2s`: all eight
limited workloads and six calibration processes passed in approximately 133
seconds. All six multi-core workloads sustained more than one CPU and passed both
whole-phase and observed rolling-window checks. This is a harness diagnostic,
not completion of the new five-minute multi-core validation.

After removing long-case warmup and establishing observation before releasing the
controller and workers, the same eight limited cases and six calibrations passed
the two-second diagnostic again in approximately 101 seconds. Each limited report
has `Warmup: 0`, with startup included in its eight-second measurement. A unit test
also verifies startup work is counted. Full-duration startup-inclusive five-minute
results remain pending; the earlier warmed-up run is not evidence for that scope.
