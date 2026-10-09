#!/usr/bin/env bash
#
# Runs ghoti-bench on AWS EC2 and downloads the results.
#
# It creates, in the default VPC of the region:
#   - a server instance running Ghoti (default t4g.micro),
#   - a generator instance running ghoti-bench against it (default c7g.xlarge),
# both in the same subnet, plus a temporary key pair and security group. It
# runs the benchmark over SSH, downloads everything into results/ec2-<time>/
# and deletes every resource it created, also when it fails or is interrupted.
#
# Usage:
#   scripts/ec2-bench.sh [ghoti-bench run flags...]
#
# Examples:
#   scripts/ec2-bench.sh                                   # full memory-read matrix (~1h)
#   scripts/ec2-bench.sh --repetitions 1 --warmup 5s --duration 30s
#   GHOTI_REF=90b5271 scripts/ec2-bench.sh --scenario memory-mixed
#   SCENARIOS="memory-read counter-hot" scripts/ec2-bench.sh --repetitions 1
#
# Settings (environment variables):
#   SCENARIOS        scenarios to run one after the other on the same
#                    instances, each into results/ec2-<time>/<scenario>/
#                    (empty: the --scenario argument, into results/ec2-<time>/)
#   GHOTI_REF        Ghoti release tag or commit to benchmark   (v0.2.0)
#   GHOTI_REPO       Ghoti repository URL or local path for commits
#   SERVER_TYPE      Ghoti server instance type                (t4g.micro)
#   GENERATOR_TYPE   ghoti-bench instance type                 (c7g.xlarge)
#   T_CREDITS        CPU credits for T instances: unlimited or standard
#                    (unlimited, so the server is not throttled mid-run when
#                    its credits run out; it may add a small surplus charge)
#   AWS_REGION       region (AWS_DEFAULT_REGION or the CLI default)
#   SSH_CIDR         addresses allowed to SSH in (your public IP/32)
#   RESULTS_ROOT     local directory for the results           (results)
#   KEEP=1           keep the instances running after the benchmark
#
# Requires: aws (v2, with credentials), go, ssh, scp and curl.

set -euo pipefail

GHOTI_REF=${GHOTI_REF:-v0.3.0}
GHOTI_REPO=${GHOTI_REPO:-https://github.com/dankomiocevic/ghoti}
SERVER_TYPE=${SERVER_TYPE:-t4g.micro}
GENERATOR_TYPE=${GENERATOR_TYPE:-c7g.xlarge}
T_CREDITS=${T_CREDITS:-unlimited}
SCENARIOS=${SCENARIOS:-}
RESULTS_ROOT=${RESULTS_ROOT:-results}
KEEP=${KEEP:-0}
GHOTI_PORT=9090
METRICS_PORT=9100

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
RUN_ID="ec2-$(date -u +%Y%m%dT%H%M%SZ)"
NAME="ghoti-bench-${RUN_ID}"
OUT="${RESULTS_ROOT}/${RUN_ID}"
WORK=$(mktemp -d)
KEY="${WORK}/key.pem"

log() { printf '\n==> %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

for tool in aws go ssh scp curl; do
  command -v "$tool" >/dev/null || die "$tool is required"
done
if [[ -n "$SCENARIOS" ]]; then
  for arg in "$@"; do
    [[ $arg == --scenario || $arg == --scenario=* ]] && die "use SCENARIOS or --scenario, not both"
  done
  # A misspelled scenario is caught here, not after the instances are up.
  KNOWN=$(cd "$REPO_ROOT" && go run ./cmd/ghoti-bench scenarios | awk '{print $1}')
  for scenario in $SCENARIOS; do
    grep -qx -- "$scenario" <<<"$KNOWN" || die "unknown scenario $scenario, available: $(echo $KNOWN)"
  done
fi

export AWS_REGION=${AWS_REGION:-${AWS_DEFAULT_REGION:-$(aws configure get region || true)}}
[[ -n "$AWS_REGION" ]] || die "no region: set AWS_REGION"
export AWS_PAGER=""
aws sts get-caller-identity --query Account --output text >/dev/null || die "AWS credentials are not configured"

# --- Resources created, removed by cleanup ----------------------------------

KEY_NAME=""
SG_ID=""
SERVER_ID=""
GENERATOR_ID=""
SERVER_IP=""
GENERATOR_IP=""
BENCH_STARTED=0
GATHERED=0

cleanup() {
  local status=$?
  trap - EXIT INT TERM
  set +e

  # Interrupted or failed after the benchmark started: keep what exists.
  if [[ $BENCH_STARTED == 1 && $GATHERED == 0 ]]; then
    log "Downloading partial results"
    gather
  fi

  local instances=()
  [[ -n "$SERVER_ID" ]] && instances+=("$SERVER_ID")
  [[ -n "$GENERATOR_ID" ]] && instances+=("$GENERATOR_ID")

  if [[ $KEEP == 1 ]]; then
    log "KEEP=1, leaving the resources running. Remove them with:"
    [[ ${#instances[@]} -gt 0 ]] && echo "  aws ec2 terminate-instances --region $AWS_REGION --instance-ids ${instances[*]}"
    [[ -n "$SG_ID" ]] && echo "  aws ec2 delete-security-group --region $AWS_REGION --group-id $SG_ID   # after the instances terminate"
    [[ -n "$KEY_NAME" ]] && echo "  aws ec2 delete-key-pair --region $AWS_REGION --key-name $KEY_NAME"
    mkdir -p "$OUT" && cp "$KEY" "${OUT}/key.pem" && echo "  SSH key: ${OUT}/key.pem (ssh -i ${OUT}/key.pem ec2-user@<ip>)"
    [[ -n "$SERVER_IP" ]] && echo "  server:    ec2-user@$SERVER_IP"
    [[ -n "$GENERATOR_IP" ]] && echo "  generator: ec2-user@$GENERATOR_IP"
    rm -rf "$WORK"
    exit "$status"
  fi

  log "Cleaning up"
  if [[ ${#instances[@]} -gt 0 ]]; then
    aws ec2 terminate-instances --instance-ids "${instances[@]}" >/dev/null
    aws ec2 wait instance-terminated --instance-ids "${instances[@]}"
    echo "terminated ${instances[*]}"
  fi
  if [[ -n "$SG_ID" ]]; then
    # The network interfaces can take a moment to go after termination.
    for _ in 1 2 3 4 5 6; do
      aws ec2 delete-security-group --group-id "$SG_ID" 2>/dev/null && { echo "deleted $SG_ID"; break; }
      sleep 10
    done
  fi
  if [[ -n "$KEY_NAME" ]]; then
    aws ec2 delete-key-pair --key-name "$KEY_NAME" && echo "deleted key pair $KEY_NAME"
  fi
  rm -rf "$WORK"
  exit "$status"
}
trap cleanup EXIT INT TERM

SSH_OPTS=(-i "$KEY" -o StrictHostKeyChecking=accept-new -o "UserKnownHostsFile=${WORK}/known_hosts"
  -o ConnectTimeout=10 -o ServerAliveInterval=30 -o LogLevel=ERROR)
on() { local host=$1; shift; ssh "${SSH_OPTS[@]}" "ec2-user@${host}" "$@"; }
put() { scp -q "${SSH_OPTS[@]}" "$2" "ec2-user@${1}:$3"; }
get() { scp -q -r "${SSH_OPTS[@]}" "ec2-user@${1}:$2" "$3"; }

# --- Instance types and images ------------------------------------------------

# arch prints the EC2 architecture of an instance type: arm64 or x86_64.
arch() {
  aws ec2 describe-instance-types --instance-types "$1" \
    --query 'InstanceTypes[0].ProcessorInfo.SupportedArchitectures[0]' --output text
}
vcpus() {
  aws ec2 describe-instance-types --instance-types "$1" --query 'InstanceTypes[0].VCpuInfo.DefaultVCpus' --output text
}
goarch() { [[ $1 == arm64 ]] && echo arm64 || echo amd64; }
ami() {
  aws ssm get-parameter --name "/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-$1" \
    --query Parameter.Value --output text
}

log "Region $AWS_REGION: server $SERVER_TYPE, generator $GENERATOR_TYPE, Ghoti $GHOTI_REF"
SERVER_ARCH=$(arch "$SERVER_TYPE")
GENERATOR_ARCH=$(arch "$GENERATOR_TYPE")
SERVER_VCPUS=$(vcpus "$SERVER_TYPE")
SERVER_AMI=$(ami "$SERVER_ARCH")
GENERATOR_AMI=$(ami "$GENERATOR_ARCH")

# --- Binaries -------------------------------------------------------------------

log "Building ghoti-bench for linux/$(goarch "$GENERATOR_ARCH")"
(cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=$(goarch "$GENERATOR_ARCH") \
  go build -trimpath -o "${WORK}/ghoti-bench" ./cmd/ghoti-bench)

log "Fetching Ghoti $GHOTI_REF for linux/$(goarch "$SERVER_ARCH")"
FETCHED=$(cd "$REPO_ROOT" && go run ./cmd/ghoti-bench fetch --ghoti-ref "$GHOTI_REF" --ghoti-repo "$GHOTI_REPO" \
  --os linux --arch "$(goarch "$SERVER_ARCH")" --output "${WORK}/ghoti")
GHOTI_VERSION=$(sed -n 's/^version=//p' <<<"$FETCHED")
GHOTI_COMMIT=$(sed -n 's/^commit=//p' <<<"$FETCHED")
echo "version ${GHOTI_VERSION:-?} commit ${GHOTI_COMMIT:-?}"

# --- Network ----------------------------------------------------------------------

VPC_ID=$(aws ec2 describe-vpcs --filters Name=isDefault,Values=true --query 'Vpcs[0].VpcId' --output text)
[[ "$VPC_ID" != None ]] || die "no default VPC in $AWS_REGION"

# Both instances go in one availability zone that offers both types, so the
# measured latency does not include a cross-AZ hop.
offered() {
  aws ec2 describe-instance-type-offerings --location-type availability-zone \
    --filters "Name=instance-type,Values=$1" --query 'InstanceTypeOfferings[].Location' --output text | tr '\t' '\n' | sort
}
AZ=$(comm -12 <(offered "$SERVER_TYPE") <(offered "$GENERATOR_TYPE") | head -n1)
[[ -n "$AZ" ]] || die "no availability zone offers both $SERVER_TYPE and $GENERATOR_TYPE"
SUBNET_ID=$(aws ec2 describe-subnets \
  --filters "Name=vpc-id,Values=$VPC_ID" "Name=availability-zone,Values=$AZ" Name=default-for-az,Values=true \
  --query 'Subnets[0].SubnetId' --output text)
[[ "$SUBNET_ID" != None ]] || die "no default subnet in $AZ"

SSH_CIDR=${SSH_CIDR:-$(curl -fsS https://checkip.amazonaws.com | tr -d '[:space:]')/32}
log "Creating key pair and security group in $VPC_ID ($AZ), SSH from $SSH_CIDR"

TAGS="{Key=Project,Value=ghoti-bench},{Key=RunId,Value=${RUN_ID}}"
KEY_NAME=$NAME
aws ec2 create-key-pair --key-name "$KEY_NAME" --key-type ed25519 \
  --tag-specifications "ResourceType=key-pair,Tags=[${TAGS}]" \
  --query KeyMaterial --output text >"$KEY"
chmod 600 "$KEY"

SG_ID=$(aws ec2 create-security-group --group-name "$NAME" --vpc-id "$VPC_ID" \
  --description "ghoti-bench ${RUN_ID}, temporary" \
  --tag-specifications "ResourceType=security-group,Tags=[${TAGS}]" --query GroupId --output text)
aws ec2 authorize-security-group-ingress --group-id "$SG_ID" --protocol tcp --port 22 --cidr "$SSH_CIDR" >/dev/null
# Ghoti and its metrics are only reachable from the other instance.
aws ec2 authorize-security-group-ingress --group-id "$SG_ID" \
  --ip-permissions "IpProtocol=tcp,FromPort=${GHOTI_PORT},ToPort=${METRICS_PORT},UserIdGroupPairs=[{GroupId=${SG_ID}}]" >/dev/null

# --- Instances ----------------------------------------------------------------------

launch() {
  local type=$1 image=$2 role=$3
  local extra=()
  [[ $type == t* ]] && extra=(--credit-specification "CpuCredits=${T_CREDITS}")
  aws ec2 run-instances --image-id "$image" --instance-type "$type" --key-name "$KEY_NAME" \
    --network-interfaces "DeviceIndex=0,SubnetId=${SUBNET_ID},Groups=${SG_ID},AssociatePublicIpAddress=true" \
    --metadata-options HttpTokens=required \
    --tag-specifications "ResourceType=instance,Tags=[{Key=Name,Value=${NAME}-${role}},${TAGS}]" \
      "ResourceType=volume,Tags=[${TAGS}]" \
    ${extra[@]+"${extra[@]}"} --query 'Instances[0].InstanceId' --output text
}

log "Launching instances"
SERVER_ID=$(launch "$SERVER_TYPE" "$SERVER_AMI" server)
GENERATOR_ID=$(launch "$GENERATOR_TYPE" "$GENERATOR_AMI" generator)
echo "server $SERVER_ID, generator $GENERATOR_ID"
aws ec2 wait instance-running --instance-ids "$SERVER_ID" "$GENERATOR_ID"

ip_of() {
  aws ec2 describe-instances --instance-ids "$1" --query "Reservations[0].Instances[0].$2" --output text
}
SERVER_IP=$(ip_of "$SERVER_ID" PublicIpAddress)
SERVER_PRIVATE_IP=$(ip_of "$SERVER_ID" PrivateIpAddress)
GENERATOR_IP=$(ip_of "$GENERATOR_ID" PublicIpAddress)

wait_ssh() {
  for _ in $(seq 60); do
    on "$1" true 2>/dev/null && return 0
    sleep 5
  done
  die "SSH to $1 did not come up"
}
log "Waiting for SSH ($SERVER_IP, $GENERATOR_IP)"
wait_ssh "$SERVER_IP"
wait_ssh "$GENERATOR_IP"

# --- Ghoti server ---------------------------------------------------------------------

log "Starting Ghoti on $SERVER_PRIVATE_IP:$GHOTI_PORT"
{
  printf '# Generated by scripts/ec2-bench.sh\naddr: "0.0.0.0:%s"\nprotocol: standard\n' "$GHOTI_PORT"
  printf 'log:\n  level: warn\n  format: text\n'
  printf 'metrics:\n  enabled: true\n  addr: "0.0.0.0:%s"\n' "$METRICS_PORT"
  # Simple memory slots 000-099 and atomic counters 100-199 cover every
  # scenario, so one server can run them all.
  for i in $(seq 0 99); do printf 'slot_%03d:\n  kind: simple_memory\n' "$i"; done
  for i in $(seq 100 199); do printf 'slot_%03d:\n  kind: atomic\n' "$i"; done
} >"${WORK}/config.yaml"
put "$SERVER_IP" "${WORK}/ghoti" ghoti
put "$SERVER_IP" "${WORK}/config.yaml" config.yaml
# HOME points at /opt/ghoti so only this config.yaml can be picked up.
on "$SERVER_IP" "sudo bash -s" <<'EOF'
set -euo pipefail
mkdir -p /opt/ghoti
install -m 0755 /home/ec2-user/ghoti /opt/ghoti/ghoti
install -m 0644 /home/ec2-user/config.yaml /opt/ghoti/config.yaml
systemd-run --unit=ghoti --property=LimitNOFILE=1048576 --property=WorkingDirectory=/opt/ghoti \
  --setenv=HOME=/opt/ghoti /opt/ghoti/ghoti run
for _ in $(seq 50); do
  curl -fsS -o /dev/null localhost:9100/metrics && exit 0
  sleep 0.2
done
journalctl -u ghoti --no-pager | tail -20
exit 1
EOF

# --- Results -------------------------------------------------------------------------

# gather downloads everything worth keeping. It is also called by cleanup
# when the run fails or is interrupted after the benchmark started.
gather() {
  mkdir -p "${OUT}/server" "${OUT}/generator"
  get "$GENERATOR_IP" results/. "$OUT/" 2>/dev/null || echo "no results on the generator"
  get "$GENERATOR_IP" bench.log "${OUT}/generator/" 2>/dev/null
  on "$GENERATOR_IP" "uname -a; echo; lscpu; echo; free -m" >"${OUT}/generator/system.txt" 2>/dev/null
  on "$SERVER_IP" "sudo journalctl -u ghoti --no-pager" >"${OUT}/server/server.log" 2>/dev/null
  on "$SERVER_IP" "cat /opt/ghoti/config.yaml" >"${OUT}/server/config.yaml" 2>/dev/null
  on "$SERVER_IP" "/opt/ghoti/ghoti version; echo; uname -a; echo; lscpu; echo; free -m" >"${OUT}/server/system.txt" 2>/dev/null
  on "$SERVER_IP" "curl -fsS localhost:${METRICS_PORT}/metrics" >"${OUT}/server/metrics-final.txt" 2>/dev/null
  cat >"${OUT}/environment.txt" <<EOF
run_id=${RUN_ID}
region=${AWS_REGION}
availability_zone=${AZ}
subnet=${SUBNET_ID}
ghoti_ref=${GHOTI_REF}
ghoti_version=${GHOTI_VERSION}
ghoti_commit=${GHOTI_COMMIT}
server_instance=${SERVER_ID} ${SERVER_TYPE} ${SERVER_ARCH} ${SERVER_AMI} ${SERVER_PRIVATE_IP:-}
server_cpu_credits=$([[ $SERVER_TYPE == t* ]] && echo "$T_CREDITS" || echo n/a)
generator_instance=${GENERATOR_ID} ${GENERATOR_TYPE} ${GENERATOR_ARCH} ${GENERATOR_AMI}
scenarios=${SCENARIOS}
ghoti_bench_command=${BENCH_CMD}
EOF
  GATHERED=1
}

# --- Benchmark -------------------------------------------------------------------------

put "$GENERATOR_IP" "${WORK}/ghoti-bench" ghoti-bench
on "$GENERATOR_IP" "timeout 5 bash -c '</dev/tcp/${SERVER_PRIVATE_IP}/${GHOTI_PORT}'" ||
  die "the generator cannot reach Ghoti at ${SERVER_PRIVATE_IP}:${GHOTI_PORT}"

BENCH_ARGS=(run --target "${SERVER_PRIVATE_IP}:${GHOTI_PORT}" --metrics-addr "${SERVER_PRIVATE_IP}:${METRICS_PORT}"
  --server-cores "$SERVER_VCPUS" --server-instance "$SERVER_TYPE" --generator-instance "$GENERATOR_TYPE"
  --region "$AWS_REGION")
[[ -n "$GHOTI_VERSION" ]] && BENCH_ARGS+=(--ghoti-version "$GHOTI_VERSION")
[[ -n "$GHOTI_COMMIT" ]] && BENCH_ARGS+=(--ghoti-commit "$GHOTI_COMMIT")
if [[ -z "$SCENARIOS" ]]; then
  BENCH_CMD=$(printf '%q ' ./ghoti-bench "${BENCH_ARGS[@]}" --results-dir results "$@")
else
  # Every scenario gets its own directory. They run in order and stop at the
  # first one that fails, the server is the same for all of them.
  BENCH_CMD=""
  for scenario in $SCENARIOS; do
    BENCH_CMD+="$(printf '%q ' ./ghoti-bench "${BENCH_ARGS[@]}" "$@" --scenario "$scenario" --results-dir "results/${scenario}")&& "
  done
  BENCH_CMD+="true"
fi

# The benchmark runs detached so a dropped SSH connection does not stop it;
# its exit status lands in bench.exit.
log "Running: ${BENCH_CMD}"
on "$GENERATOR_IP" "nohup bash -c $(printf '%q' "{ ${BENCH_CMD}; } >bench.log 2>&1; echo \$? >bench.exit") </dev/null >/dev/null 2>&1 &"
BENCH_STARTED=1

shown=0
unreachable=0
status=""
while [[ -z "$status" ]]; do
  sleep 15
  # Print the log lines that are new since the last poll.
  if ! lines=$(on "$GENERATOR_IP" "tail -n +$((shown + 1)) bench.log 2>/dev/null || true"); then
    unreachable=$((unreachable + 1))
    # 40 failed polls is 10 minutes without reaching the generator.
    [[ $unreachable -lt 40 ]] || die "lost contact with the generator"
    continue
  fi
  unreachable=0
  if [[ -n "$lines" ]]; then
    printf '%s\n' "$lines"
    shown=$((shown + $(printf '%s\n' "$lines" | wc -l)))
  fi
  status=$(on "$GENERATOR_IP" "cat bench.exit 2>/dev/null || true") || status=""
done

log "Downloading results to $OUT"
gather
ls -1 "$OUT"

[[ "$status" == 0 ]] || die "ghoti-bench exited with status $status, see ${OUT}/generator/bench.log"
if [[ -z "$SCENARIOS" ]]; then
  log "Done: ${OUT}/summary.csv"
else
  log "Done: $(for scenario in $SCENARIOS; do printf '%s ' "${OUT}/${scenario}/summary.csv"; done)"
fi
