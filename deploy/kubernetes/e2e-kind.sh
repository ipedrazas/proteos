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
#   deploy/kubernetes/e2e-kind.sh --cni=cilium
#       # K6: a Cilium cluster instead of kind's default CNI, the agent with
#       # PROTEOS_K8S_FQDN_POLICY=cilium, and the FQDN egress test enabled.
#       # Needs the `cilium` CLI (or CILIUM_BIN); slower, and not the CI job.
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
CNI=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --keep) KEEP=1; shift ;;
    --no-build) BUILD=0; shift ;;
    --reuse) REUSE=1; shift ;;
    --cni=cilium) CNI=cilium; shift ;;
    --cni=*) die "unknown CNI '${1#--cni=}' (only cilium is supported)" ;;
    -h | --help) sed -n '2,30p' "$0"; exit 0 ;;
    *) die "unknown arg: $1" ;;
  esac
done

for bin in kind kubectl docker go openssl; do
  command -v "$bin" >/dev/null || die "$bin is required"
done
CILIUM_BIN="${CILIUM_BIN:-cilium}"
if [[ $CNI == cilium ]]; then
  command -v "$CILIUM_BIN" >/dev/null || die "--cni=cilium needs the cilium CLI (set CILIUM_BIN, or https://github.com/cilium/cilium-cli/releases)"
fi

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
      log "--- machine pod detail (conditions + probe failures) ---"
      kubectl -n "$MACHINE_NS" describe pods 2>&1 | sed 's/^/    /' || true
      log "--- machine namespace events ---"
      # Events are where a pod that never becomes ready explains itself:
      # scheduling, image pulls, volume attach, and failing probes.
      kubectl -n "$MACHINE_NS" get events --sort-by=.lastTimestamp 2>&1 | tail -40 | sed 's/^/    /' || true
      log "--- networkpolicies ---"
      kubectl -n "$MACHINE_NS" get networkpolicies -o yaml 2>&1 | tail -60 | sed 's/^/    /' || true
      log "--- cluster-agent logs ---"
      kubectl -n "$AGENT_NS" logs deploy/proteos-cluster-agent --tail=60 2>&1 | sed 's/^/    /' || true
    fi
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT

# --- 1. cluster -------------------------------------------------------------
# With --cni=cilium the cluster is created WITHOUT kind's default CNI and
# Cilium is installed into it: the FQDN engine (K6) writes CiliumNetworkPolicy
# objects, and only Cilium's DNS proxy can make toFQDNs follow a name. kind's
# own CNI enforces NetworkPolicy (the K5 probe passes on it) but has no
# FQDN-aware policy of any kind.
create_cluster() {
  if [[ $CNI == cilium ]]; then
    cat >"$WORK/kind.yaml" <<'YAML'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  disableDefaultCNI: true
YAML
    # No --wait: nodes cannot be Ready before a CNI exists.
    kind create cluster --name "$CLUSTER" --config "$WORK/kind.yaml"
    log "installing Cilium (this pulls its images; a few minutes the first time)"
    "$CILIUM_BIN" install --wait >"$WORK/cilium-install.log" 2>&1 \
      || { tail -20 "$WORK/cilium-install.log"; die "cilium install failed"; }
    "$CILIUM_BIN" status --wait >/dev/null || die "cilium never became ready"
    kubectl wait --for=condition=Ready node --all --timeout=180s >/dev/null
    ok "Cilium installed ($("$CILIUM_BIN" version 2>/dev/null | grep -m1 'cilium image (running)' || true))"
  else
    kind create cluster --name "$CLUSTER" --wait 120s
  fi
}
if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  if [[ $REUSE -eq 1 ]]; then
    log "reusing existing cluster '$CLUSTER'"
  else
    log "deleting stale cluster '$CLUSTER'"
    kind delete cluster --name "$CLUSTER" >/dev/null
    create_cluster
  fi
else
  log "creating kind cluster '$CLUSTER'"
  create_cluster
fi
kubectl config use-context "kind-$CLUSTER" >/dev/null

# Machine pods are Guaranteed-QoS on memory and request a share of a core per
# vcpu, so what the node can actually offer decides whether one can be scheduled
# at all. Print it: "Insufficient cpu" is otherwise a puzzle rather than a fact.
log "node capacity ($(nproc 2>/dev/null || echo '?') host cores):"
kubectl get nodes -o custom-columns='NAME:.metadata.name,CPU:.status.allocatable.cpu,MEM:.status.allocatable.memory' \
  2>&1 | sed 's/^/    /' || true

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
if [[ $CNI == cilium ]]; then
  # What a Cilium cluster needs on top of the base, applied the way an operator
  # would rather than through a third overlay: the CiliumNetworkPolicy that
  # lets the agent reach the API server at all (a NetworkPolicy ipBlock does
  # not match cluster nodes on Cilium — without it the agent never becomes
  # ready and its log is empty), and the FQDN engine. The agent's preflight
  # checks that the CRD is served and refuses to start otherwise.
  kubectl apply -f "$SCRIPT_DIR/base/networkpolicy-cilium.yaml" >/dev/null || die "apply of the Cilium agent policy failed"
  kubectl -n "$AGENT_NS" set env deploy/proteos-cluster-agent PROTEOS_K8S_FQDN_POLICY=cilium >/dev/null
fi

if [[ $REUSE -eq 1 ]]; then
  # A fresh token was written into the Secret above, and a Secret change does
  # not restart the pods that read it into their environment: on a reused
  # cluster the running agent would keep answering 401 with the old one.
  kubectl -n "$AGENT_NS" rollout restart deploy/proteos-cluster-agent >/dev/null
fi
log "waiting for the cluster-agent to become ready"
if ! kubectl -n "$AGENT_NS" rollout status deploy/proteos-cluster-agent --timeout=180s; then
  kubectl -n "$AGENT_NS" describe deploy/proteos-cluster-agent | tail -30
  kubectl -n "$AGENT_NS" logs deploy/proteos-cluster-agent --tail=80 || true
  FAILED=1
  die "the cluster-agent never became ready (its startup preflight may have refused: RBAC, namespace, RuntimeClass)"
fi
ok "cluster-agent ready"

# The agent only becomes ready after its preflight, and the preflight's last
# step is the NetworkPolicy enforcement probe (K5): a canary pod, denied all
# ingress by policy, that the agent must fail to reach. kind ≥ 0.24 enforces
# policy out of the box (kube-network-policies), so on this cluster the probe
# must have PASSED — and its verdict is the only place CI asserts that the CNI
# under test actually blocks pod-to-machine traffic, rather than that the driver
# merely writes the objects. Reaching "ready" already proves the agent did not
# refuse to start; this proves it did not skip the question.
#
# By label, not `logs deploy/`: after a rollout (a reused cluster) a pod from
# the previous ReplicaSet can linger in Terminating, `logs deploy/` may pick it
# and lose it mid-call, and the verdict would be missed on a healthy agent. So
# wait for the old pods to go, then read every agent pod's log.
agent_pods() { kubectl -n "$AGENT_NS" get pods -l app.kubernetes.io/name=proteos-cluster-agent --no-headers 2>/dev/null; }
for _ in $(seq 1 45); do
  agent_pods | grep -q Terminating || break
  sleep 2
done
if ! kubectl -n "$AGENT_NS" logs -l app.kubernetes.io/name=proteos-cluster-agent --tail=-1 | grep -q "NetworkPolicy enforcement verified"; then
  kubectl -n "$AGENT_NS" logs -l app.kubernetes.io/name=proteos-cluster-agent --tail=40 || true
  FAILED=1
  die "the cluster-agent came up without verifying NetworkPolicy enforcement"
fi
ok "NetworkPolicy enforcement verified by the agent's canary"

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
      PROTEOS_E2E_FQDN="$([[ $CNI == cilium ]] && echo 1 || true)" \
      go test -tags=kind_e2e -count=1 -failfast -timeout 35m -v ./internal/driver/kubernetes/ -run 'TestKind'
)
RC=$?
set -e
if [[ $RC -ne 0 ]]; then
  FAILED=1
  die "the kind_e2e suite failed (rc=$RC)"
fi

# --- 6b. the control plane in front of it -----------------------------------
# The suite above drives the agent as the control plane would. This one IS the
# control plane: an in-process instance with the cluster-agent as its only host
# (driver=kubernetes) and an image-only template, so the K3 placement path, the
# poweroff-not-hibernate stop, and the machine-web preview proxy with both port
# allowlists run against a real machine. It pins the agent's certificate the way
# production does (PROTEOS_NODE_CA_FILE), which is why the cert path is passed.
#
# Postgres comes from testcontainers (or TEST_DATABASE_URL when set), so this
# needs docker — which it has, since kind does too.
log "running the control-plane kind_e2e suite"
set +e
(
  cd "$REPO_ROOT/controlplane" &&
    PROTEOS_E2E_AGENT_URL="https://127.0.0.1:$LOCAL_PORT" \
      PROTEOS_E2E_AGENT_TOKEN="$TOKEN" \
      PROTEOS_E2E_AGENT_CA="$WORK/tls.crt" \
      PROTEOS_E2E_GUEST_IMAGE="$GUEST_IMAGE" \
      go test -tags=kind_e2e -count=1 -failfast -timeout 30m -v ./internal/httpapi/ -run 'TestKind'
)
RC=$?
set -e
if [[ $RC -ne 0 ]]; then
  FAILED=1
  die "the control-plane kind_e2e suite failed (rc=$RC)"
fi

# --- 7. no leaks ------------------------------------------------------------
# Every test destroys its machine, so the namespace must end up empty. A pod or
# PVC left behind means Destroy is not the cleanup the control plane believes
# it is.
#
# But deletion is not instant: a pod lives out its termination grace period and
# a PVC is pinned by its pvc-protection finalizer until the pod releases it, so
# for a few seconds after the last test both are still listed as Terminating.
# Counting those as leaks is the same mistake the driver used to make — treating
# "still visible to a Get" as "still exists" — so wait for the deletions to
# actually complete. That is the stronger check anyway: it catches a destroy
# that is wedged on a finalizer, which a snapshot count never would.
log "waiting for destroyed machine objects to finish terminating"
leak_count() {
  # secrets: the per-machine guest listener token (K5) leaves with Destroy too.
  # The netpol canary also carries the managed-by label, so a probe that failed
  # to clean up after itself is caught here as well. On Cilium the machine's
  # CiliumNetworkPolicy (K6) is one more object Destroy must take with it.
  local kinds=pods,pvc,networkpolicies,secrets
  [[ $CNI == cilium ]] && kinds="$kinds,ciliumnetworkpolicies"
  kubectl -n "$MACHINE_NS" get "$kinds" \
    -l app.kubernetes.io/managed-by=proteos --no-headers 2>/dev/null | grep -c . || true
}
for _ in $(seq 1 60); do
  [[ "$(leak_count)" == "0" ]] && break
  sleep 2
done

LEAKED="$(leak_count)"
if [[ "$LEAKED" != "0" ]]; then
  kubectl -n "$MACHINE_NS" get pods,pvc,networkpolicies,secrets -l app.kubernetes.io/managed-by=proteos
  FAILED=1
  die "$LEAKED machine object(s) still present two minutes after Destroy"
fi
ok "no machine objects leaked"

printf '\n\e[1;32m[e2e-kind] the Kubernetes backend works against a real cluster\e[0m\n'
