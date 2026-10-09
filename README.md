# ghoti-bench

Benchmarks for [Ghoti](https://github.com/dankomiocevic/ghoti).

ghoti-bench does not import the Ghoti Go module. It talks to Ghoti only the
way any other client can: the TCP protocol, and the Prometheus endpoint for
server-side statistics. It can benchmark any version, including ones that
change internal packages, and the numbers cannot be influenced by sharing code
with the server.

## Usage

```sh
make build            # bin/ghoti-bench
```

```sh
ghoti-bench run --ghoti-ref v0.2.0                 # downloads and verifies the release
ghoti-bench run --ghoti-ref 90b5271                # builds that commit from source
ghoti-bench run --ghoti-binary ./ghoti             # uses an existing binary
ghoti-bench run --target 10.0.1.15:9090 --metrics-addr 10.0.1.15:9100 \
  --ghoti-version 0.2.0 --ghoti-commit 90b5271
ghoti-bench scenarios                              # lists the scenarios
ghoti-bench fetch --ghoti-ref v0.2.0 --os linux --arch arm64 --output ghoti
```

ghoti-bench gets a server (release, commit or binary), starts a fresh one for
every run, and runs a scenario over a matrix of connection counts and
repetitions.

- **Tags** are downloaded from the GitHub release for this OS and architecture,
  checked against `checksums.txt`. If there is no release archive (or with
  `--build`) the ref is built from source.
- **Commits and branches** are built from a cached clone (`--ghoti-repo` can
  point to a local checkout) with the release flags: `CGO_ENABLED=0`,
  `-trimpath`, `-s -w`.
- Downloads, sources and builds are cached in the user cache directory
  (`--cache-dir`).
- When ghoti-bench starts the server it writes its `config.yaml` (only the
  slots of the scenario, log level `warn`, metrics on a free port), runs it in
  its own directory with `HOME` pointing there so no user configuration is
  picked up, and starts a new server for every run. `--server-gomaxprocs`
  limits the server cores.

Defaults run the full matrix: `--connections 1,100,1000`, `--repetitions 3`,
`--warmup 30s`, `--duration 5m`. Repetitions are the outer loop, so drift of
the host spreads over every connection count.

With `--target` the server is not started or restarted by ghoti-bench, so
it must already have the scenario slots configured with the kind the scenario
needs: `simple_memory` for the memory scenarios and `atomic` for the counter
scenarios. Ghoti
cannot be asked for its version over the network, so pass `--ghoti-version`
and `--ghoti-commit` to record which Ghoti was measured.

A started server gets free ports; if another process takes one before Ghoti
binds it, the start is retried on new ports. After every run the server must
exit cleanly on SIGTERM: a server that crashed during the run, exited with an
error or had to be killed stops the benchmark, because the next run would no
longer be isolated.

## Running on EC2

`scripts/ec2-bench.sh` runs the benchmark on two EC2 instances in the default
VPC: a Ghoti server (`t4g.micro` by default) and a bigger generator
(`c7g.xlarge`), in the same subnet. It needs the AWS CLI v2 with credentials,
Go, ssh and curl.

```sh
scripts/ec2-bench.sh                                     # full memory-read matrix, about 1 hour
scripts/ec2-bench.sh --repetitions 1 --warmup 5s --duration 30s
GHOTI_REF=90b5271 SERVER_TYPE=t3.micro GENERATOR_TYPE=c7i.xlarge scripts/ec2-bench.sh
SCENARIOS="memory-read memory-mixed" scripts/ec2-bench.sh
scripts/ec2-counter-bench.sh                             # counter-hot and counter-spread, about 2 hours
```

`SCENARIOS` runs several scenarios one after the other on the same instances,
which is cheaper than starting new ones for each and keeps the hardware the
same for all of them. Each scenario is written to its own directory,
`results/ec2-<timestamp>/<scenario>/`, and the first one that fails stops the
rest. `scripts/ec2-counter-bench.sh` is `ec2-bench.sh` with `SCENARIOS` set to
the counter scenarios, and takes the same settings and arguments.

The script:

1. Cross-compiles ghoti-bench for the generator and gets Ghoti for the server
   with `ghoti-bench fetch` (the verified release, or a build of the commit).
2. Creates a temporary key pair and security group (SSH from your IP only,
   Ghoti and metrics only between the two instances) and launches both
   instances on Amazon Linux 2023 in one availability zone. T instances use
   `unlimited` CPU credits (`T_CREDITS`) so the server is not throttled
   halfway through.
3. Starts Ghoti as a systemd unit with simple memory slots `000-099`, atomic
   counter slots `100-199` and metrics enabled, so one server can run every
   scenario,
   then runs `ghoti-bench run --target` on the generator, detached so a
   dropped SSH connection does not stop it, and streams its log.
4. Downloads `summary.csv`, the run JSONs, the benchmark log, the Ghoti
   journal and config, the final metrics and both hosts' CPU and memory
   details into `results/ec2-<timestamp>/`.
5. Terminates the instances and deletes the security group and key pair, also
   when it fails or is interrupted (partial results are downloaded first).
   `KEEP=1` leaves everything running and prints the commands to remove it.

Every resource is tagged `Project=ghoti-bench` and `RunId=ec2-<timestamp>`.
Arguments are passed to `ghoti-bench run`. See the header of the script for
every setting.

## Scenarios

### memory-read

The first benchmark: closed-loop reads of a simple memory slot.

1. Slot `000` is preloaded with a known 36-byte value.
2. Every connection is opened before the run and stays open.
3. Each connection repeatedly sends `r000\n`, waits for the complete response
   and checks it byte for byte against `v000<value>\n`. Only one request is in
   flight per connection.

It measures Ghoti's protocol and connection overhead without mutation, rate
limiting, broadcasting or clustering in the way.

### memory-mixed

95% reads and 5% writes over slots `000-099`, slot chosen uniformly. Every
write to a slot stores the value it was preloaded with, so every response,
read or write, is still validated exactly while the server does the full
write path.

### counter-hot

Every connection increments the same atomic counter, slot `100`.

1. Slot `100` is set to `0` before the run.
2. Each connection repeatedly sends `r100\n`, which increments the counter and
   returns the new value.
3. The value is shared by every connection, so it cannot be known exactly in
   advance. A connection only has one request in flight and Ghoti increments
   the counter under a lock, so every value a connection reads has to be
   greater than the previous one it read. A value that does not grow is
   counted as an incorrect response.
4. After the run, if there were no errors, the counter is read once more. It
   has to hold exactly the number of increments the generator validated: a
   lower value means increments were lost and a higher one that some were
   applied twice. A mismatch makes the run invalid.

It measures how the counter behaves when every connection contends for the
same slot.

### counter-spread

The same as `counter-hot` over slots `100-199`, slot chosen uniformly. With
100 slots there is little contention on any one of them, so comparing it with
`counter-hot` shows how much the contention on a single slot costs.

Counters only use reads. A write sets the counter to an absolute value, which
would make the values a connection reads jump backwards and the final check
impossible.

## Method

- **Closed loop.** One request in flight per connection, as the Ghoti
  protocol requires. Throughput is the number of *validated* responses whose
  whole round trip fell inside the measurement window, divided by the window.
- **Warm-up.** Requests during the warm-up are sent and validated but not
  measured. The window starts at `warmup` and lasts `duration`.
- **No per-request overhead in the generator.** Requests and expected
  responses are prebuilt byte slices, the read buffer is reused, there is no
  per-request logging or allocation, and the read deadline is only moved
  forward once half of it is used.
- **Latency** is recorded in a per-connection
  [HDR histogram](https://hdrhistogram.github.io/HdrHistogram/) (1µs–timeout,
  3 significant digits), merged after the run. The max is exact.
  `--save-histogram` embeds the compressed histogram in the JSON.
- **Errors** are timeouts, connection errors, `e` responses and incorrect
  responses. A connection that fails is reopened after an exponential backoff
  (10ms doubling to 1s, reset by the next validated response), so a failing
  server is not hammered with reconnects. Async `a` lines are skipped.
- **Server stats** are sampled every second on the same clock as the load:
  CPU and RSS from the process (`/proc` on Linux, `ps` on macOS) or from
  Prometheus `process_*` metrics, connections from `ghoti_connected_clients`.
  CPU percentages are relative to all the server cores (100% = saturated).
  `ghoti_requests_total` is kept as a cross-check of the generator count.
- **Validity.** A run is `valid` only if every connection was established,
  there were no errors at all (warm-up included), it was not interrupted, the
  server reported every connection for the whole measurement, the counters
  matched the validated increments (counter scenarios), and the generator CPU stayed under
  `--max-generator-cpu` (80%). Past that the generator may be what is being
  measured. The reasons are listed in `run.json`. The table printed at the
  end aggregates only valid runs (`N/A` when there are none) and lists the
  invalid ones with their reasons.

Running the generator and the server on the same host is fine for comparing
commits, but they compete for CPU: publishable numbers need separate hosts.

## Output

Each run adds one row to `summary.csv` and writes `<run-id>.json`:

```
results/<timestamp>-memory-read/
  summary.csv                    one row per run, for comparing and graphing
  memory-read-c1000-r1.json      configuration, full results and timelines
  servers/memory-read-c1000-r1/  generated config.yaml and server.log
```

`summary.csv` columns:

```
run_id,timestamp,scenario,server_instance,generators,connections,duration_s,total_requests,throughput_rps,latency_p50_ms,latency_p95_ms,latency_p99_ms,latency_p999_ms,max_latency_ms,errors,error_rate,server_cpu_avg_pct,server_cpu_max_pct,server_rss_mb,generator_cpu_max_pct,valid
```

Server columns are empty when they could not be collected. `run.json` adds the
error breakdown, reads and writes, the counter check, p90, mean and stddev, generator CPU and
network peak (application bytes), the Ghoti version and commit, the per-second
load and server timelines, and the reasons for an invalid run. Nothing is
written per request.

Label the hardware with `--server-instance`, `--generator-instance` and
`--region`.
