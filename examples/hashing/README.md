# Hashing example

This portable example creates CPU work by repeatedly feeding small chunks into
one SHA-256 state per worker. Workers share one limiter and call `AwaitCapacity`
before each chunk. A root context stops the controller and workers when the
duration expires or you press Ctrl+C. No input file is required.

Run from the repository root:

```sh
go run ./examples/hashing -cpu-percent=5 -mode=single -duration=1m
```

This targets 5% of one logical CPU. To target a percentage of combined logical
CPU capacity, select `-mode=all`. That mode also divides the base sampling
interval by `runtime.NumCPU()`.

Select the power-curve policy:

```sh
go run ./examples/hashing -policy=power -alpha=2 -cpu-percent=5 -duration=1m
```

The power policy uses a soft ratio of 0.8 and no probability floor. Alpha 1 is
linear, values greater than 1 throttle more aggressively, and values between
0 and 1 throttle less aggressively. Both policies use weight 1 and caller
fraction 1. These do not grant each worker a separate CPU budget.

For a baseline workload comparison:

```sh
go run ./examples/hashing -unlimited -workers=2 -duration=10s
```

The example defaults to two workers, 256 KiB chunks, a 30-second averaging
window, a 100 ms base sampling interval, and a one-minute duration. Its admission
margin is 5% of the global allowance, rather than the library's default .02 CPU.
These settings illustrate usage; they are not benchmark-validated recommendations.

Use `go run ./examples/hashing -help` for all flags. In particular, `-window`,
`-sample-every`, `-chunk-bytes`, and `-workers` let you explore responsiveness and
concurrency. Keep each chunk's computation short enough for prompt throttling and
cancellation. Longer windows and concurrent chunks can allow temporary spikes.

Output includes the workload PID, progress, and a final digest of each worker's
processed stream. Digests vary with the number of chunks completed. Progress is
throughput, not a measurement of CPU usage; this is an example, not an E2E test.

On macOS, monitor the printed workload PID in another terminal:

```sh
top -pid <PID> -l 60 -s 1 -stats pid,command,cpu,time
```

When using `go run`, monitor the PID printed by the example, not the Go launcher.
The workload is shared across macOS, Linux, and Windows; independent CPU
measurement tools and their options are platform-specific.

See the [initial macOS validation report](../../docs/benchmark-results.md) for
measured throttling results, reproduction commands, and their limitations.
