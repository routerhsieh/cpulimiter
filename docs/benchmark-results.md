# Initial macOS validation results

This report records manual end-to-end validation of the hashing example before
the initial v0.1.0 release. It is a baseline for future automated E2E tests, not
a performance guarantee or a comprehensive benchmark suite.

## Environment and setup

- Report added: October 4, 2026.
- Test host: macOS, Apple Silicon (`arm64`), 14 logical CPUs.
- Host environment recorded when adding this report: macOS 26.6.2 (25G83),
  Go 1.26.6 (`darwin/arm64`).
- Two workers, each incrementally hashing an in-memory stream in 256 KiB chunks.
- One shared limiter, caller fraction 1 and weight 1 for each worker.
- Base sampling interval: 100 ms. AllCores effective interval on this host:
  approximately 7.14 ms.
- Admission margin: the example's 5% of the global allowance, not the library's
  default 0.02 CPU.
- Power curve: soft ratio 0.8, alpha 2, no probability floor.
- Runs executed sequentially; unrelated machine activity was not controlled.

No production code was changed for these experiments. See the
[hashing example](../examples/hashing/README.md) for its workload and options.

## Independent measurements

The workload was compiled once and executed under `/usr/bin/time -p`. Overall
process usage below is `100 * (user + sys) / real`, expressed as a percentage of
one core. CPU durations are rounded to hundredths of a second by the CLI. The
measurement includes the entire process, including the limiter and reporting.

macOS `top` independently collected approximately one-second CPU readings for
the workload PID, excluding its initial snapshot. These observations cover
portions of the runs; their means need not match the whole-run measurements.
For the long run, cumulative CPU time from `top` and high-resolution elapsed
timestamps were also used to estimate trailing five-minute averages. These
sampled windows are approximate, not proof of an exact rolling-window guarantee.

## Results

| Run | Measured wall time | Window | Target | CPU seconds | Mean, one-core reference |
| --- | --- | --- | --- | --- | --- |
| Unthrottled | 30.28 s | N/A | N/A | 60.03 | 198.25% |
| Deterministic, SingleCore | 90 s | 30 s | 5% of one core | 4.34 | 4.82% |
| PowerCurve, SingleCore | 90 s | 30 s | 5% of one core | 3.73 | 4.14% |
| Deterministic, SingleCore | 420 s | 300 s | 5% of one core | 20.09 | 4.78% |
| Deterministic, AllCores | 30 s | 10 s | 1% of 14 cores = 14% of one core | 4.01 | 13.37% |

The AllCores result corresponds to approximately 0.955% of combined capacity.
For example, the long run used 19.63 seconds of user CPU and 0.46 seconds of
system CPU: `100 * (19.63 + 0.46) / 420 = 4.78%` of one core.

### `top` observations

- Unthrottled: mean 197.34%, maximum sampled CPU 200.6%.
- Deterministic / 30-second window: mean 4.70%, maximum sampled CPU 18.5%.
- PowerCurve / 30-second window: mean 4.09%, maximum sampled CPU 22.9%.
- Deterministic / five-minute window: mean 4.71%, maximum sampled CPU 18.7%.
- AllCores: mean 13.28%, maximum sampled CPU 15.2% (one-core reference).

The seven-minute run yielded 16 estimated trailing five-minute windows after
the observer had collected enough history. Their maximum was 4.775%; the final
one was 4.771%. The observer covered part of the run, so these do not exhaust
every possible five-minute window. Instantaneous peaks may also be missed by
one-second observation intervals.

All runs completed with exit status 0 and both workers made progress. The
deterministic policy visibly produced bursts separated by pauses; the power
policy generally made smaller increments more frequently after startup. It did
not eliminate spikes or establish a lower peak than deterministic admission.

A separate 30-second-duration run was interrupted with Ctrl+C after about 6.6
seconds. Both workers and the controller shut down, printed final digests, and
the process exited with status 0.

## Reproduce on macOS

Run from the repository root. Build first so the timed commands measure the
workload process rather than `go run` compilation or launcher overhead:

```sh
go build -o /tmp/cpulimiter-hashing ./examples/hashing

# Unthrottled baseline.
/usr/bin/time -p /tmp/cpulimiter-hashing -unlimited -workers=2 -chunk-bytes=262144 -duration=30s

# Deterministic admission, short window.
/usr/bin/time -p /tmp/cpulimiter-hashing -workers=2 -chunk-bytes=262144 -duration=90s -window=30s -sample-every=100ms -cpu-percent=5 -mode=single -policy=deterministic

# Power-curve admission, same target and window.
/usr/bin/time -p /tmp/cpulimiter-hashing -workers=2 -chunk-bytes=262144 -duration=90s -window=30s -sample-every=100ms -cpu-percent=5 -mode=single -policy=power -alpha=2

# Deterministic admission, five-minute window.
/usr/bin/time -p /tmp/cpulimiter-hashing -workers=2 -chunk-bytes=262144 -duration=7m -window=5m -sample-every=100ms -cpu-percent=5 -mode=single -policy=deterministic

# Combined logical CPU capacity.
/usr/bin/time -p /tmp/cpulimiter-hashing -workers=2 -chunk-bytes=262144 -duration=30s -window=10s -sample-every=100ms -cpu-percent=1 -mode=all -policy=deterministic
```

In another terminal, replace `12345` below with the PID printed by the workload:

```sh
top -pid 12345 -l 600 -s 1 -stats pid,cpu,time
```

Ignore the first `top` snapshot. Stop the observer after the workload exits.
The `cpu` column uses a one-core reference: 100% means one fully occupied core,
not the entire machine. On a host with N logical CPUs, convert to combined
capacity percentage by dividing by N. The AllCores target therefore depends
on the machine's logical CPU count.

To estimate a trailing-window average independently, record elapsed timestamps
alongside cumulative process CPU times. For two observations approximately
300 seconds apart, calculate `100 * (CPU_end - CPU_start) / (wall_end - wall_start)`.
The commands above expose the measurements, but do not automate window analysis
or pass/fail assertions.

To check interruption, run the following and press Ctrl+C while it is running:

```sh
/tmp/cpulimiter-hashing -workers=2 -duration=30s -window=5s -cpu-percent=5 -mode=single -policy=deterministic
```

Expect both workers to report final digests and the process to exit cleanly.
CPU readings, throughput, and digests will vary between runs and machines;
identical numbers are not expected.

## Scope and limitations

These experiments demonstrate useful sustained throttling and clean shutdown
for this workload on this macOS host. Admission is cooperative: workers must
call `AwaitCapacity` regularly between bounded chunks of work. Temporary CPU
spikes are expected; this is not an instantaneous ceiling or an exact SLA.

Only one workload and one host were tested. These results do not validate
Linux or Windows runtime behavior, all worker counts or chunk sizes, or the
library's default configuration (the example uses its own admission margin).
Automated E2E tests, broader workloads, and default tuning remain future work.
