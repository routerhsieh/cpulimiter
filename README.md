# cpulimiter

A cooperative CPU limiter for Go applications. It measures process CPU usage
and asks participating workers to pause between chunks of work when the recent
average approaches a configured target.

It is intended for daemons and other long-running applications that want to
reduce background CPU consumption while keeping control loops and essential
tasks running. Unlike an OS-enforced limit, it only delays work that explicitly
calls `AwaitCapacity`; it does not suspend the whole process.

## Requirements and installation

- Minimum compatible Go version: **1.22**. A currently supported Go toolchain
  is recommended.
- Built-in process CPU meters for macOS, Linux, Windows, and FreeBSD.
- Real-workload throttling has been validated on macOS. Linux, Windows, and
  FreeBSD runtime validation is still pending.

Once the repository is published, install it with:

```sh
go get github.com/routerhsieh/cpulimiter
```

The import path is `github.com/routerhsieh/cpulimiter`; the Go package name is
`cpulimit`.

## Minimal usage

Share one limiter across CPU-intensive workers. Start its controller once with
the application's root context, and pass that context or a descendant to every
`AwaitCapacity` call.

```go
package main

import (
    "context"
    "crypto/sha256"
    "time"

    cpulimit "github.com/routerhsieh/cpulimiter"
)

func main() {
    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()

    limiter := cpulimit.New(cpulimit.Config{
        Mode:        cpulimit.SingleCore,
        GlobalFrac:  0.05, // 5% of one logical CPU, shared by all workers.
        Window:      30 * time.Second,
        SampleEvery: 100 * time.Millisecond,
        Hysteresis:  0.0025, // Margin: 5% of this example's allowance.
    }, nil, nil) // nil selects the OS meter and deterministic admission.

    controllerDone := make(chan struct{})
    go func() {
        defer close(controllerDone)
        limiter.Run(ctx)
    }()

    // Real applications can run multiple workers using this same limiter.
    chunk := make([]byte, 64*1024)
    hash := sha256.New()
    for {
        if err := limiter.AwaitCapacity(ctx, 1); err != nil {
            break
        }
        _, _ = hash.Write(chunk)
    }

    cancel()
    <-controllerDone
}
```

`AwaitCapacity` returns immediately when admission succeeds. Otherwise it waits
for another admission opportunity or returns the context cancellation error.
Keep each work chunk short enough for responsive throttling and cancellation.
The [hashing example](examples/hashing/README.md) demonstrates multiple workers,
progress reporting, admission policies, and Ctrl+C shutdown.

## Configuration

CPU usage is measured in logical CPU units: `1.0` means one fully occupied
logical CPU. `GlobalFrac` is a fraction, not a percentage.

| Setting | Default | Meaning |
| --- | --- | --- |
| `Mode` | `SingleCore` | Reference capacity for the target and sampling cadence. |
| `GlobalFrac` | `0.70` | Fraction of the reference capacity allowed to the process. |
| `Window` | `5 * time.Minute` | Approximate history window for the sample average. |
| `SampleEvery` | `100 * time.Millisecond` | Base CPU sampling interval. |
| `Hysteresis` | `0.02` | Admission margin in CPU units, not a fraction of the target. |
| `Aging.Enabled` | `false` | Temporarily relax admission for blocked calls. |
| `Aging.Delay` | `0` | Waiting time before aging starts; zero starts immediately. |
| `Aging.RampDuration` | `30 * time.Second` | Time after the delay to reach the maximum boost. |
| `Aging.MaxFracBoost` | `0.1` | Maximum additive boost to the caller's fraction. |

`SingleCore` uses `GlobalFrac` directly as the CPU allowance. `AllCores` uses
`GlobalFrac * runtime.NumCPU()` and divides the base sampling interval by that
logical CPU count. For example, `GlobalFrac: 0.05` allows 5% of one core in
SingleCore mode, or 5% of combined logical CPU capacity in AllCores mode.

The ring buffer stores `ceil(Window / effective sampling interval)` equally
weighted samples. This approximates a time window; delayed samples, failed
meter reads, and sampling-cadence changes affect its actual time coverage.
AllCores mode increases sampling overhead and history capacity on larger hosts.

Nonpositive numeric settings above, except `Aging.Delay`, use their defaults; NaN and infinity also default
for floating-point settings. In particular, `Hysteresis: 0` selects the default
margin rather than disabling it. Modes other than `AllCores` behave as SingleCore.
The margin supplied to a policy is capped at half the caller's allowance.
For small CPU targets, consider an explicitly smaller margin, as in the example.
Negative `Aging.Delay` becomes zero. Positive finite boosts above 1 are accepted;
the effective caller fraction remains capped at 1.

### Optional aging

Aging is disabled by default. Enable it to give blocked callers more admission
opportunities at the cost of temporarily narrowing their original priority gaps:

```go
cfg.Aging = cpulimit.AgingConfig{
    Enabled:      true,
    Delay:        5 * time.Second,
    RampDuration: 30 * time.Second,
    MaxFracBoost: 0.1,
}
```

After the first rejected admission attempt, each `AwaitCapacity` call tracks its
own elapsed waiting time. On each retry the limiter computes:

```text
progress = clamp((waited - Delay) / RampDuration, 0, 1)
effectiveFrac = min(1, normalizedFrac + MaxFracBoost * progress)
```

With the settings above, a caller with `frac = 0.3` stays at 0.3 for five seconds,
reaches 0.35 after twenty seconds, and reaches 0.4 after thirty-five seconds.
Success or cancellation discards the boost; the next call starts fresh.
Aging changes the allowance passed to either built-in policy, without changing
the optional probability weight. Custom policies decide how to use that allowance.
Retries normally happen on recorded samples, so aging is not an independent timer;
failed readings or a stopped controller can prevent retries.

Aging does not guarantee progress, waiter ordering, CPU shares, or Go scheduler
priority. It never raises the caller's allowance above the global allowance;
sampling delays, concurrent work chunks, and the power curve's upper margin can
still allow temporary overshoot. Leave aging disabled when preserving the original
priority thresholds is essential.

### Runtime updates

`SetConfig` queues a complete configuration replacement. `Run` applies it
asynchronously; the newest pending update replaces an older pending update.
`GetConfig` returns the applied configuration, not necessarily a pending one.

When the window or effective sampling interval changes, the limiter resizes its
history, preserving the newest samples that fit. Other updates also retain
history. Changing cadence does not reweight existing samples or recover samples
already discarded.
Aging updates take effect on the next admission attempt using the original wait
start, including time spent waiting while aging was disabled. Disabling aging
removes the boost on the next attempt.

## Admission policies

A nil policy in `New` selects `admission.Deterministic{}`. It admits when the
process average is at or below the caller's allowance minus the margin.

For probabilistic admission, pass a power-curve policy:

```go
policy := admission.NewPowerCurve(admission.PowerCurveConfig{
    SoftRatio:   0.8,
    Alpha:       2,
    MinPassProb: 0,
})
limiter := cpulimit.New(cfg, nil, policy)
```

Import `github.com/routerhsieh/cpulimiter/admission` for this snippet. The power
curve admits immediately below its soft threshold minus the margin, decreases
admission probability as usage approaches the allowance, and blocks at the
allowance plus the margin. Alpha 1 gives a linear curve; larger values throttle
more aggressively between the thresholds, and values between 0 and 1 less
aggressively. A positive probability floor may admit above the nominal allowance
within the upper margin.

`AwaitCapacity(ctx, frac, weight...)` scales the global allowance by `frac`.
Use fractions in `(0, 1]`; 1 uses the full global allowance. Finite values above
1 are now clamped to 1; nonpositive or nonfinite values use 0.000001.
A smaller fraction makes that caller wait at a lower
process-wide average; it does **not** reserve an independent share for a worker
or measure that worker's CPU usage. The optional weight defaults to 1; the power
curve uses it as a probability multiplier, while deterministic admission ignores
it. Pass at most one weight. Neither setting guarantees fairness.

Custom policies implement:

```go
type AdmissionPolicy interface {
    CanProceed(admission.State) bool
}
```

The state contains the process average, caller allowance, admission margin, and
weight. Policies run under the limiter's mutex: they must return promptly,
avoid blocking I/O, and never call back into the limiter. A policy shared across
limiters must support concurrent calls.

The policy instance is fixed at construction. Policies can expose their own
synchronized tuning API; the built-in power curve provides `Update` to replace
its settings. Workers observe changes on their next admission attempt, normally
after a sampling wakeup. Cancellation remains the limiter's responsibility.

## Limitations and validation

- This is best-effort cooperative throttling, not a hard CPU ceiling or an exact
  rolling-window SLA. Startup, concurrent work chunks, and sampling delays can
  produce temporary spikes.
- Measurement includes all process CPU, but only workers calling
  `AwaitCapacity` are throttled. Ungated work can exceed the target on its own.
- Admission does not reserve capacity or preempt work already running. Avoid
  long CPU-heavy operations between checks.
- Start `Run` exactly once per instance. When its context is canceled, workers
  must also be canceled; do not leave workers waiting on unrelated contexts.
- Meter failures skip samples and retain the previous average. There is no
  public meter-error reporting API or guaranteed throttle response during a
  measurement outage.

The [initial macOS validation report](docs/benchmark-results.md) records real
hashing workloads, independent CLI CPU measurements, and reproduction commands.
It demonstrates sustained throttling and clean shutdown for the tested
configurations, not all workloads or platforms. Automated E2E tests remain
future work.

Run the unit tests and race checks with:

```sh
go test ./...
go test -race ./...
```

## Development

This project was developed with AI assistance, including OpenAI Codex, for
design discussions, implementation, tests, and documentation. The maintainer
makes the design decisions and is responsible for reviewing changes and
maintaining the project.

## License

[MIT](LICENSE).
