#!/usr/bin/env bash
#
# Runs the token bucket benchmarks on AWS EC2 and downloads the results.
#
# It is scripts/ec2-bench.sh with SCENARIOS set to the token bucket
# scenarios, so both run on the same pair of instances, one after the other,
# and each one gets its own directory in results/ec2-<time>/. With the
# default matrix every scenario takes about 55 minutes.
#
# Usage:
#   scripts/ec2-token-bucket-bench.sh [ghoti-bench run flags...]
#
# Examples:
#   scripts/ec2-token-bucket-bench.sh                      # token-bucket-hot and token-bucket-spread (~2h)
#   scripts/ec2-token-bucket-bench.sh --repetitions 1 --warmup 5s --duration 30s
#   SCENARIOS=token-bucket-hot scripts/ec2-token-bucket-bench.sh
#
# Settings (environment variables):
#   SCENARIOS        token bucket scenarios to run   (token-bucket-hot token-bucket-spread)
#   Every other setting is the same as in scripts/ec2-bench.sh.

set -euo pipefail

export SCENARIOS=${SCENARIOS:-token-bucket-hot token-bucket-spread}
exec "$(dirname "${BASH_SOURCE[0]}")/ec2-bench.sh" "$@"
