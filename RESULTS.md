# Ghoti Benchmarks

These are the results of benchmarking Ghoti on AWS EC2 with [ghoti-bench](README.md). They compare Ghoti v0.2.0 with v0.3.0, and the different slot types on v0.3.0. Every number on this page can be reproduced with the commands in [How to reproduce](#how-to-reproduce).

## Summary

- **v0.3.0 is faster than v0.2.0.** With 100 connections on a t4g.micro it serves 134,343 requests per second against 78,185, 71.8% more. With 1000 connections it serves 112,647 against 63,186, 78.3% more.
- **Latency is lower too.** With 100 connections, 99% of the requests take less than 3.54 ms on v0.3.0 and 5.30 ms on v0.2.0.
- **Counters and token buckets cost the same as memory reads.** On v0.3.0 every slot type is within 5% of plain memory reads, even when every connection uses the same slot.
- **A bigger instance does not help.** v0.2.0 on a t4g.small, with twice the memory of a t4g.micro and the same 2 cores, serves 80,291 requests per second with 100 connections, 2.7% more. The server is limited by CPU.
- **Every result is valid.** All 39 runs finished with 0 errors over 524,263,392 requests, and every counter and token bucket gave the right answers.

## What was measured

Ghoti runs alone on a small server instance, a t4g.micro with 2 cores. A much bigger instance in the same zone opens a number of connections to it (1, 100 or 1000) and sends requests as fast as the server answers them. Each connection waits for the answer to its request before it sends the next one, so the number of requests per second is what the server can sustain with that many clients.

|Scenario |What every request does |Runs |
|---|---|---|
|memory-read |Reads a 36 byte value from a simple memory slot. |3 runs of 5 minutes |
|counter-hot |Increments one atomic counter that every connection shares. |1 run of 30 seconds |
|counter-spread |Increments one of 100 atomic counters, chosen at random. |1 run of 30 seconds |
|token-bucket-hot |Takes a token from one token bucket that every connection shares, 1,000 tokens per second. |1 run of 30 seconds |
|token-bucket-spread |Takes a token from one of 100 token buckets, chosen at random, 10 tokens per second each. |1 run of 30 seconds |

The charts show 100 and 1000 connections, where the server is the limit. With a single connection the result depends mostly on the network between the two instances, see [Single connections](#single-connections).

## v0.3.0 against v0.2.0

memory-read, the median of three runs.

```text
100 connections

v0.2.0 on t4g.micro  ██████████████████▋                78,185 req/s
v0.2.0 on t4g.small  ███████████████████▏               80,291 req/s
v0.3.0 on t4g.micro  ████████████████████████████████  134,343 req/s
```

```text
1,000 connections

v0.2.0 on t4g.micro  ██████████████████                 63,186 req/s
v0.2.0 on t4g.small  ██████████████████▍                64,896 req/s
v0.3.0 on t4g.micro  ████████████████████████████████  112,647 req/s
```

|Version and server |Connections |Requests per second |Change |p50 ms |p99 ms |Server CPU |
|---|--:|--:|--:|--:|--:|--:|
|v0.2.0 on t4g.micro |100 |78,185 |- |1.026 |5.304 |96% |
|v0.2.0 on t4g.small |100 |80,291 |+2.7% |1.015 |5.095 |95% |
|v0.3.0 on t4g.micro |100 |134,343 |+71.8% |0.600 |3.541 |82% |
|v0.2.0 on t4g.micro |1,000 |63,186 |- |14.926 |33.243 |92% |
|v0.2.0 on t4g.small |1,000 |64,896 |+2.7% |14.533 |32.670 |92% |
|v0.3.0 on t4g.micro |1,000 |112,647 |+78.3% |8.376 |18.694 |89% |

The change is against v0.2.0 on a t4g.micro with the same number of connections. Server CPU is the average use of both cores during the run.

## Slot types

Ghoti v0.3.0 on a t4g.micro. memory-read is the median of three runs, the other slot types one run each.

```text
100 connections

memory-read          ██████████████████████████████▊   134,343 req/s
counter-hot          █████████████████████████████▎    127,594 req/s
counter-spread       ██████████████████████████████▉   134,746 req/s
token-bucket-hot     ████████████████████████████████  139,552 req/s
token-bucket-spread  ███████████████████████████████▉  139,188 req/s
```

```text
1,000 connections

memory-read          ███████████████████████████████▉  112,647 req/s
counter-hot          ███████████████████████████████▏  110,026 req/s
counter-spread       ████████████████████████████████  112,955 req/s
token-bucket-hot     ████████████████████████████████  113,173 req/s
token-bucket-spread  ████████████████████████████████  113,062 req/s
```

|Scenario |Connections |Requests per second |Against memory-read |p50 ms |p99 ms |Server CPU |
|---|--:|--:|--:|--:|--:|--:|
|memory-read |100 |134,343 |- |0.600 |3.541 |82% |
|counter-hot |100 |127,594 |-5.0% |0.628 |4.053 |78% |
|counter-spread |100 |134,746 |+0.3% |0.602 |3.494 |83% |
|token-bucket-hot |100 |139,552 |+3.9% |0.588 |3.379 |86% |
|token-bucket-spread |100 |139,188 |+3.6% |0.591 |3.238 |86% |
|memory-read |1,000 |112,647 |- |8.376 |18.694 |89% |
|counter-hot |1,000 |110,026 |-2.3% |8.552 |19.300 |88% |
|counter-spread |1,000 |112,955 |+0.3% |8.385 |17.990 |90% |
|token-bucket-hot |1,000 |113,173 |+0.5% |8.339 |18.711 |89% |
|token-bucket-spread |1,000 |113,062 |+0.4% |8.397 |17.842 |90% |

Differences of a few percent between slot types are within what one 30 second run can show, so the slot types should be read as equal.

## Correctness checks

The value of a counter or a token bucket changes on every request, so the answers cannot be checked one by one the way a memory read is. They are checked after the run instead:

- **Counters** are read back once the run is over. A counter has to hold exactly the number of increments the benchmark counted, otherwise some were lost or applied twice.
- **Token buckets** have to hand out every token they refill while the load keeps them empty, and never more than they can hold plus their refills.

|Scenario |Connections |Result |
|---|--:|---|
|counter-hot |1 |Passed. The counter matched all 120,912 increments. |
|counter-hot |100 |Passed. The counter matched all 4,514,037 increments. |
|counter-hot |1,000 |Passed. The counter matched all 3,850,751 increments. |
|counter-spread |1 |Passed. The counter matched all 116,246 increments. |
|counter-spread |100 |Passed. The counter matched all 4,716,734 increments. |
|counter-spread |1,000 |Passed. The counter matched all 3,956,127 increments. |
|token-bucket-hot |1 |Passed. 29,977 tokens in the 30 second window. Over the whole 35 second run the bucket handed out 36,270 tokens, with 33,000 to 38,000 allowed. |
|token-bucket-hot |100 |Passed. 30,000 tokens in the 30 second window. Over the whole 35 second run the bucket handed out 36,999 tokens, with 33,000 to 38,000 allowed. |
|token-bucket-hot |1,000 |Passed. 30,000 tokens in the 30 second window. Over the whole 35 second run the bucket handed out 36,999 tokens, with 33,000 to 38,000 allowed. |
|token-bucket-spread |1 |Passed. 29,988 tokens in the 30 second window. Over the whole 35 second run the 100 buckets handed out 36,875 tokens, with 330 to 380 allowed for each one. |
|token-bucket-spread |100 |Passed. 30,000 tokens in the 30 second window. Over the whole 35 second run the 100 buckets handed out 36,900 tokens, with 330 to 380 allowed for each one. |
|token-bucket-spread |1,000 |Passed. 30,000 tokens in the 30 second window. Over the whole 35 second run the 100 buckets handed out 36,900 tokens, with 330 to 380 allowed for each one. |

## Single connections

With a single connection the server is idle most of the time, waiting for the next request, so the result is mostly the network round trip between the two instances. v0.2.0 ran at 7,964 requests per second (p50 0.121 ms) and v0.3.0 at 3,359 (p50 0.290 ms) on the same instance types, in different sessions. The gap most likely comes from the network path each pair of instances got, not from Ghoti, so the versions are compared with 100 and 1000 connections. The single connection numbers are in [All results](#all-results).

## How to reproduce

1. Install the AWS CLI v2 with credentials for an account that has a default VPC in the region, plus Go, ssh, scp and curl.
2. Clone ghoti-bench and run the command of the session you want to repeat from its root.
3. The script creates both instances, runs the benchmark, downloads everything to `results/ec2-<timestamp>/` and deletes every resource it created, also when it fails. A full memory session takes about an hour, a slot type session a few minutes.
4. Compare your `summary.csv` with [All results](#all-results).

|Session |Command |
|---|---|
|v0.2.0 on t4g.micro |`AWS_REGION=us-west-2 GHOTI_REF=v0.2.0 scripts/ec2-bench.sh` |
|v0.2.0 on t4g.small |`AWS_REGION=us-west-2 GHOTI_REF=v0.2.0 SERVER_TYPE=t4g.small scripts/ec2-bench.sh` |
|v0.3.0 on t4g.micro |`AWS_REGION=us-west-2 GHOTI_REF=v0.3.0 scripts/ec2-bench.sh` |
|Counters on v0.3.0 t4g.micro |`AWS_REGION=us-west-2 GHOTI_REF=v0.3.0 scripts/ec2-counter-bench.sh --repetitions 1 --warmup 5s --duration 30s` |
|Token buckets on v0.3.0 t4g.micro |`AWS_REGION=us-west-2 GHOTI_REF=v0.3.0 scripts/ec2-token-bucket-bench.sh --repetitions 1 --warmup 5s --duration 30s` |

Every session ran on a c7g.xlarge generator in us-west-2. The memory sessions ran three repetitions of 5 minutes after a 30 second warm-up, and the slot type sessions one of 30 seconds after a 5 second warm-up. Each session used a build of ghoti-bench from before its scripts were merged, so a run from `main` can differ slightly.

To try a scenario on your own machine, `ghoti-bench run --ghoti-ref v0.3.0 --scenario counter-hot` starts the server itself. Numbers from one host are only good for comparing commits, because the server and the benchmark compete for the same CPU.

## How the results were obtained

- **Hosts.** Two instances in the same subnet of one availability zone, on Amazon Linux 2023. The server is a t4g.micro (2 Graviton2 cores, 1 GiB), or a t4g.small (2 cores, 2 GiB) where it says so, with unlimited CPU credits so it is never throttled. The load comes from a c7g.xlarge (4 Graviton3 cores), much bigger than the server so the benchmark is not what is being measured.
- **Server.** The released Ghoti binary for linux/arm64, checked against the release checksums, with log level warn and metrics enabled. It has simple memory slots 000-099, and atomic counters 100-199 and token buckets 200 and 300-399 in the sessions that used them.
- **Load.** Every connection is opened before the run and stays open. Requests and their expected answers are prepared before the run, so the benchmark does no work per request beyond sending it and checking the answer. A warm-up runs first and is not measured.
- **Latency.** Every round trip is recorded in a histogram with 3 significant digits. p50 is the time half of the requests stay under, p99 the time 99% of them stay under. The maximum is exact.
- **Validity.** A run only counts if every connection stayed open for the whole run, there were no errors at all, the correctness checks passed and the benchmark used less than 80% of its CPU. The most it used in any run here was 42.2%.

## All results

The median of the valid runs for every case. Spread is half the range of the runs relative to the median, and max is the slowest round trip of any run.

### Throughput

|Scenario |Version and server |Connections |Runs |Requests per second |Lowest and highest run |Spread |Server CPU |Errors |
|---|---|--:|--:|--:|--:|--:|--:|--:|
|memory-read |v0.2.0 on t4g.micro |1 |3 |7,964 |7,925 to 8,286 |2.3% |26.6% |0 |
|memory-read |v0.2.0 on t4g.micro |100 |3 |78,185 |77,758 to 79,338 |1.0% |96.2% |0 |
|memory-read |v0.2.0 on t4g.micro |1,000 |3 |63,186 |63,152 to 63,725 |0.5% |91.7% |0 |
|memory-read |v0.2.0 on t4g.small |1 |3 |3,812 |3,793 to 3,851 |0.8% |13.3% |0 |
|memory-read |v0.2.0 on t4g.small |100 |3 |80,291 |80,206 to 80,614 |0.3% |95.4% |0 |
|memory-read |v0.2.0 on t4g.small |1,000 |3 |64,896 |64,819 to 64,928 |0.1% |91.5% |0 |
|memory-read |v0.3.0 on t4g.micro |1 |3 |3,359 |3,095 to 3,420 |4.8% |6.1% |0 |
|memory-read |v0.3.0 on t4g.micro |100 |3 |134,343 |132,398 to 136,914 |1.7% |82.5% |0 |
|memory-read |v0.3.0 on t4g.micro |1,000 |3 |112,647 |110,630 to 113,507 |1.3% |89.3% |0 |
|counter-hot |v0.3.0 on t4g.micro |1 |1 |3,453 |- |- |6.0% |0 |
|counter-hot |v0.3.0 on t4g.micro |100 |1 |127,594 |- |- |78.1% |0 |
|counter-hot |v0.3.0 on t4g.micro |1,000 |1 |110,026 |- |- |88.0% |0 |
|counter-spread |v0.3.0 on t4g.micro |1 |1 |3,320 |- |- |5.9% |0 |
|counter-spread |v0.3.0 on t4g.micro |100 |1 |134,746 |- |- |82.9% |0 |
|counter-spread |v0.3.0 on t4g.micro |1,000 |1 |112,955 |- |- |89.7% |0 |
|token-bucket-hot |v0.3.0 on t4g.micro |1 |1 |3,996 |- |- |6.9% |0 |
|token-bucket-hot |v0.3.0 on t4g.micro |100 |1 |139,552 |- |- |85.9% |0 |
|token-bucket-hot |v0.3.0 on t4g.micro |1,000 |1 |113,173 |- |- |89.3% |0 |
|token-bucket-spread |v0.3.0 on t4g.micro |1 |1 |3,957 |- |- |7.0% |0 |
|token-bucket-spread |v0.3.0 on t4g.micro |100 |1 |139,188 |- |- |86.3% |0 |
|token-bucket-spread |v0.3.0 on t4g.micro |1,000 |1 |113,062 |- |- |90.0% |0 |

### Latency

|Scenario |Version and server |Connections |p50 ms |p99 ms |p99.9 ms |max ms |
|---|---|--:|--:|--:|--:|--:|
|memory-read |v0.2.0 on t4g.micro |1 |0.121 |0.196 |0.353 |7.486 |
|memory-read |v0.2.0 on t4g.micro |100 |1.026 |5.304 |8.684 |217.382 |
|memory-read |v0.2.0 on t4g.micro |1,000 |14.926 |33.243 |40.337 |240.178 |
|memory-read |v0.2.0 on t4g.small |1 |0.257 |0.332 |0.544 |10.577 |
|memory-read |v0.2.0 on t4g.small |100 |1.015 |5.095 |8.315 |27.378 |
|memory-read |v0.2.0 on t4g.small |1,000 |14.533 |32.670 |39.485 |258.661 |
|memory-read |v0.3.0 on t4g.micro |1 |0.290 |0.372 |0.597 |16.829 |
|memory-read |v0.3.0 on t4g.micro |100 |0.600 |3.541 |7.123 |57.185 |
|memory-read |v0.3.0 on t4g.micro |1,000 |8.376 |18.694 |26.231 |255.356 |
|counter-hot |v0.3.0 on t4g.micro |1 |0.282 |0.368 |0.747 |9.754 |
|counter-hot |v0.3.0 on t4g.micro |100 |0.628 |4.053 |8.864 |74.566 |
|counter-hot |v0.3.0 on t4g.micro |1,000 |8.552 |19.300 |36.078 |71.779 |
|counter-spread |v0.3.0 on t4g.micro |1 |0.294 |0.369 |0.552 |5.697 |
|counter-spread |v0.3.0 on t4g.micro |100 |0.602 |3.494 |7.881 |51.752 |
|counter-spread |v0.3.0 on t4g.micro |1,000 |8.385 |17.990 |24.789 |32.151 |
|token-bucket-hot |v0.3.0 on t4g.micro |1 |0.242 |0.332 |0.681 |10.764 |
|token-bucket-hot |v0.3.0 on t4g.micro |100 |0.588 |3.379 |7.803 |65.059 |
|token-bucket-hot |v0.3.0 on t4g.micro |1,000 |8.339 |18.711 |26.329 |223.583 |
|token-bucket-spread |v0.3.0 on t4g.micro |1 |0.242 |0.368 |0.719 |8.952 |
|token-bucket-spread |v0.3.0 on t4g.micro |100 |0.591 |3.238 |7.520 |27.378 |
|token-bucket-spread |v0.3.0 on t4g.micro |1,000 |8.397 |17.842 |25.068 |222.237 |
