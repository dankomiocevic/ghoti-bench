#!/usr/bin/env bash
#
# Runs the atomic counter benchmarks on AWS EC2 and downloads the results.
#
# It is scripts/ec2-bench.sh with SCENARIOS set to the counter scenarios, so
# both run on the same pair of instances, one after the other, and each one
# gets its own directory in results/ec2-<time>/. With the default matrix
# every scenario takes about 55 minutes.
#
# Usage:
#   scripts/ec2-counter-bench.sh [ghoti-bench run flags...]
#
# Examples:
#   scripts/ec2-counter-bench.sh                           # counter-hot and counter-spread (~2h)
#   scripts/ec2-counter-bench.sh --repetitions 1 --warmup 5s --duration 30s
#   SCENARIOS=counter-hot scripts/ec2-counter-bench.sh     # only the single contended slot
#
# Settings (environment variables):
#   SCENARIOS        counter scenarios to run    (counter-hot counter-spread)
#   Every other setting is the same as in scripts/ec2-bench.sh.

set -euo pipefail

export SCENARIOS=${SCENARIOS:-counter-hot counter-spread}
exec "$(dirname "${BASH_SOURCE[0]}")/ec2-bench.sh" "$@"
