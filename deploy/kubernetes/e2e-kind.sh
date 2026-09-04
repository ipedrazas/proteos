#!/usr/bin/env bash
# e2e-kind.sh — drive the Kubernetes backend end to end against a real cluster.
#
# The kubernetes driver's unit tests run against client-go's fake clientset,
# which is a plain object store: it cannot schedule a pod, bind a PVC, pull an
# image, run a readiness probe or route a packet. Everything that makes this
# backend actually work is therefore untested until something applies the real
# manifests to a real API server. That is this script.
#
# It creates a kind cluster, builds and loads the node-agent and guest images,
# applies deploy/kubernetes/overlays/kind (which is the SHIPPING base plus two
# kind-specific overrides), port-forwards the cluster-agent, and runs the
# `kind_e2e`-tagged Go tests against it.
#
# It is a FUNCTIONAL test, not an isolation test: kind has no gVisor, so machine
# pods run on the node's default runtime. It proves the driver drives Kubernetes
# correctly; it proves nothing about the sandbox a production deployment relies
# on (see SECURITY.md).
#
# Usage:
#   deploy/kubernetes/e2e-kind.sh            # create, test, tear down
#   deploy/kubernetes/e2e-kind.sh --keep     # leave the cluster up afterwards
#   deploy/kubernetes/e2e-kind.sh --no-build # reuse already-built :e2e images
#   deploy/kubernetes/e2e-kind.sh --reuse    # reuse an existing kind cluster
#
# Requires: kind, kubectl, docker, go, openssl.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

CLUSTER="${PROTEOS_E2E_CLUSTER:-proteos-e2e}"
GUEST_IMAGE="proteos-guest:e2e"
AGENT_IMAGE="proteos-node-agent:e2e"
AGENT_NS="proteos-system"
MACHINE_NS="proteos-machines"
LOCAL_PORT="${PROTEOS_E2E_PORT:-19090}"

log() { printf '\e[1;34m[e2e-kind]\e[0m %s\n' "$*"; }
ok() { printf '\e[1;32m[ ok ]\e[0m %s\n' "$*"; }
die() {
  printf '\e[1;31m[fail]\e[0m %s\n' "$*" >&2
  exit 1
}

KEEP=0
BUILD=1
REUSE=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --keep) KEEP=1; shift ;;
    --no-build) BUILD=0; shift ;;
    --reuse) REUSE=1; shift ;;
    -h | --help) sed -n '2,26p' "$0"; exit 0 ;;
    *) die "unknown arg: $1" ;;
  esac
done

for bin in kind kubectl docker go openssl; do
  command -v "$bin" >/dev/null || die "$bin is required"
done

PF_PID=""
WORK="$(mktemp -d)"
cleanup() {
  [[ -n $PF_PID ]] && kill "$PF_PID" 2>/dev/null || true
  if [[ $KEEP -eq 1 ]]; then
    log "leaving cluster '$CLUSTER' up (--keep). Delete it with: kind delete cluster --name $CLUSTER"
  else
    # Dump what went wrong before the evidence is deleted with the cluster.
    if [[ ${FAILED:-0} -eq 1 ]]; then
      log "--- machine pods ---"
      kubectl -n "$MACHINE_NS" get pods,pvc -o wide 2>&1 | sed 's/^/    /' || true
      kubectl -n "$MACHINE_NS" describe pods 2>&1 | tail -60 | sed 's/^/    /' || true
      log "--- cluster-agent logs ---"
      kubectl -n "$AGENT_NS" logs deploy/proteos-cluster-agent --tail=100 2>&1 | sed 's/^/    /' || true
    fi
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT

# --- 1. cluster -------------------------------------------------------------
if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  if [[ $REUSE -eq 1 ]]; then
    log "reusing existing cluster '$CLUSTER'"
  else
    log "deleting stale cluster '$CLUSTER'"
    kind delete cluster --name "$CLUSTER" >/dev/null
    kind create cluster --name "$CLUSTER" --wait 120s
  fi
else
  log "creating kind cluster '$CLUSTER'"
  kind create cluster --name "$CLUSTER" --wait 120s
fi
kubectl config use-context "kind-$CLUSTER" >/dev/null

# --- 2. images --------------------------------------------------------------
# Built here and side-loaded into the node, never pulled: the overlay sets
# imagePullPolicy accordingly.
if [[ $BUILD -eq 1 ]]; then
  log "building $AGENT_IMAGE"
  docker build -q -f "$REPO_ROOT/deploy/node-agent/node-agent.Dockerfile" \
    -t "$AGENT_IMAGE" "$REPO_ROOT" >/dev/null || die "node-agent image build failed"

  log "building $GUEST_IMAGE (this pulls Claude/Node/code-server; slow the first time)"
  docker build -q -f "$REPO_ROOT/image/guest.Dockerfile" \
    --build-arg TEMPLATE=base \
    --build-arg "GUESTAGENT_VERSION=ga-$(git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null || echo nogit)" \
    -t "$GUEST_IMAGE" "$REPO_ROOT" >/dev/null || die "guest image build failed"
fi

# `kind load` writes each image out as a tar and then imports it into the node's
# containerd, so it briefly needs about three copies of the guest image (~2.7 GB)
# on disk. When that runs out the error is an unhelpful "no space left on
# device" from `docker save`, so report the headroom first.
log "loading images into the kind node (free disk: $(df -h / | awk 'NR==2 {print $4}'))"
kind load docker-image "$AGENT_IMAGE" --name "$CLUSTER" >/dev/null \
  || die "kind load of $AGENT_IMAGE failed (out of disk?): $(df -h / | awk 'NR==2 {print $4}') free"
kind load docker-image "$GUEST_IMAGE" --name "$CLUSTER" >/dev/null \
  || die "kind load of $GUEST_IMAGE failed (out of disk?): $(df -h / | awk 'NR==2 {print $4}') free"
ok "images loaded"

# --- 3. the secret ----------------------------------------------------------
# Deliberately created the way the README tells an operator to, so the e2e
# exercises the real Secret wiring (and proves TLS is genuinely on) rather than
# a simplified copy of it.
TOKEN="$(openssl rand -hex 32)"
log "generating a self-signed agent certificate"
openssl req -x509 -newkey rsa:2048 -nodes -days 2 \
  -subj "/CN=proteos-cluster-agent" \
  -addext "subjectAltName=DNS:proteos-cluster-agent,DNS:proteos-cluster-agent.${AGENT_NS}.svc,DNS:localhost,IP:127.0.0.1" \
  -keyout "$WORK/tls.key" -out "$WORK/tls.crt" >/dev/null 2>&1 || die "cert generation failed"

kubectl create namespace "$AGENT_NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n "$AGENT_NS" create secret generic proteos-cluster-agent \
  --from-literal=PROTEOS_AGENT_TOKEN="$TOKEN" \
  --from-file=tls.crt="$WORK/tls.crt" \
  --from-file=tls.key="$WORK/tls.key" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null
ok "secret created"

# --- 4. manifests -----------------------------------------------------------
log "applying deploy/kubernetes/overlays/kind"
kubectl apply -k "$SCRIPT_DIR/overlays/kind" >/dev/null || die "kubectl apply failed"

log "waiting for the cluster-agent to become ready"
if ! kubectl -n "$AGENT_NS" rollout status deploy/proteos-cluster-agent --timeout=180s; then
  kubectl -n "$AGENT_NS" describe deploy/proteos-cluster-agent | tail -30
  kubectl -n "$AGENT_NS" logs deploy/proteos-cluster-agent --tail=80 || true
  FAILED=1
  die "the cluster-agent never became ready (its startup preflight may have refused: RBAC, namespace, RuntimeClass)"
fi
ok "cluster-agent ready"

# --- 5. port-forward --------------------------------------------------------
log "port-forwarding the agent to 127.0.0.1:$LOCAL_PORT"
kubectl -n "$AGENT_NS" port-forward svc/proteos-cluster-agent "$LOCAL_PORT:9090" >"$WORK/pf.log" 2>&1 &
PF_PID=$!

for i in $(seq 1 40); do
  if curl -ksf "https://127.0.0.1:$LOCAL_PORT/healthz" >/dev/null 2>&1; then break; fi
  kill -0 "$PF_PID" 2>/dev/null || { cat "$WORK/pf.log"; FAILED=1; die "port-forward died"; }
  sleep 0.5
  [[ $i -eq 40 ]] && { cat "$WORK/pf.log"; FAILED=1; die "agent /healthz never answered through the port-forward"; }
done
ok "agent reachable over TLS"

# --- 6. the tests -----------------------------------------------------------
log "running the kind_e2e suite"
set +e
(
  cd "$REPO_ROOT/nodeagent" &&
    PROTEOS_E2E_AGENT_URL="https://127.0.0.1:$LOCAL_PORT" \
      PROTEOS_E2E_AGENT_TOKEN="$TOKEN" \
      PROTEOS_E2E_GUEST_IMAGE="$GUEST_IMAGE" \
      go test -tags=kind_e2e -count=1 -timeout 25m -v ./internal/driver/kubernetes/ -run 'TestKind'
)
RC=$?
set -e
if [[ $RC -ne 0 ]]; then
  FAILED=1
  die "the kind_e2e suite failed (rc=$RC)"
fi

# --- 7. no leaks ------------------------------------------------------------
# Every test destroys its machine, so the namespace must be empty. A leaked pod
# or PVC here means Destroy is not the cleanup the control plane believes it is.
LEAKED="$(kubectl -n "$MACHINE_NS" get pods,pvc,networkpolicies \
  -l app.kubernetes.io/managed-by=proteos --no-headers 2>/dev/null | wc -l | tr -d ' ')"
if [[ "$LEAKED" != "0" ]]; then
  kubectl -n "$MACHINE_NS" get pods,pvc,networkpolicies -l app.kubernetes.io/managed-by=proteos
  FAILED=1
  die "$LEAKED machine object(s) survived Destroy"
fi
ok "no machine objects leaked"

printf '\n\e[1;32m[e2e-kind] the Kubernetes backend works against a real cluster\e[0m\n'
