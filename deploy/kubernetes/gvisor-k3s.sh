#!/usr/bin/env bash
# gvisor-k3s.sh — install the gVisor runtime on a k3s node, so ProteOS machine
# pods can be sandboxed.
#
# WHY THIS IS NOT OPTIONAL. A machine pod runs a coding agent with
# --dangerously-skip-permissions and unauthenticated terminal / code-server /
# preview listeners. On the Firecracker backend the microVM boundary is what
# makes that acceptable: the guest has its own kernel. A pod on the node's
# shared kernel does not, and one container escape is then a whole-fleet
# compromise. gVisor puts a user-space kernel back in between. The cluster-agent
# refuses to start unless the RuntimeClass it is configured with exists, which is
# the failure this script exists to prevent.
#
# RUN IT ON THE NODE, as root, on every node machine pods may be scheduled to:
#
#   sudo deploy/kubernetes/gvisor-k3s.sh          # install, restart k3s, verify
#   sudo deploy/kubernetes/gvisor-k3s.sh --check  # verify only, change nothing
#
# It is idempotent. Debian/Ubuntu amd64 or arm64; k3s must already be installed.
#
# gVisor's default platform (systrap) needs no nested virtualisation, so this
# works inside a Proxmox/KVM guest as-is. Kata would need nested virt exposed by
# the hypervisor, which is why gVisor is the recommended runtime here.
set -euo pipefail

CHECK_ONLY=0
[[ "${1:-}" == "--check" ]] && CHECK_ONLY=1

CONTAINERD_CONFIG=/var/lib/rancher/k3s/agent/etc/containerd/config.toml
DROPIN_NAME=10-proteos-runsc.toml

log() { printf '\e[1;34m[gvisor]\e[0m %s\n' "$*"; }
ok() { printf '\e[1;32m[ ok ]\e[0m %s\n' "$*"; }
die() {
    printf '\e[1;31m[fail]\e[0m %s\n' "$*" >&2
    exit 1
}

[[ $EUID -eq 0 ]] || die "run as root (it installs a package and restarts k3s)"
command -v k3s >/dev/null || die "k3s is not installed on this node"

# --- 1. runsc ---------------------------------------------------------------
if command -v runsc >/dev/null; then
    ok "runsc already installed: $(runsc --version | head -1)"
elif [[ $CHECK_ONLY -eq 1 ]]; then
    die "runsc is not installed"
else
    log "installing runsc from the gVisor apt repository"
    apt-get update -qq
    apt-get install -y --no-install-recommends ca-certificates curl gnupg >/dev/null
    curl -fsSL https://gvisor.dev/archive.key |
        gpg --dearmor -o /usr/share/keyrings/gvisor-archive-keyring.gpg
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/gvisor-archive-keyring.gpg] https://storage.googleapis.com/gvisor/releases release main" \
        >/etc/apt/sources.list.d/gvisor.list
    apt-get update -qq
    # runsc is the sandbox; the shim is what containerd talks to.
    apt-get install -y runsc >/dev/null
    ok "installed $(runsc --version | head -1)"
fi

command -v containerd-shim-runsc-v1 >/dev/null ||
    die "containerd-shim-runsc-v1 is missing — containerd cannot launch a sandbox without it"

# k3s_unit is whichever k3s service this node runs: a server runs `k3s`, an agent
# runs `k3s-agent`, and restarting the wrong one silently does nothing.
k3s_unit() {
    for unit in k3s k3s-agent; do
        if systemctl is-active --quiet "$unit"; then
            echo "$unit"
            return 0
        fi
    done
    return 1
}

# write_dropin registers runsc through containerd's import mechanism, which is
# how k3s supports extra runtimes without owning the whole config
# (https://docs.k3s.io/advanced#configuring-containerd). Both the import
# directory and the plugin table name are READ OUT of the config k3s generated on
# this node rather than assumed: the directory moved between k3s releases
# (config.toml.d → config-v3.toml.d) and the plugin path moved between containerd
# releases (io.containerd.grpc.v1.cri → io.containerd.cri.v1.runtime). Copying
# the shape of the runc block that is already there cannot be wrong about either.
#
# This is deliberately NOT a config template. A template replaces the generated
# config wholesale, so getting it wrong for your version takes the node's entire
# containerd down; a drop-in only adds a runtime.
write_dropin() {
    local imports_line dropin_dir runc_header runsc_header
    imports_line="$(grep -m1 '^imports' "$CONTAINERD_CONFIG" || true)"
    if [[ -z $imports_line ]]; then
        die "$CONTAINERD_CONFIG has no imports line: this k3s is too old for drop-in
containerd config. Register runsc with a config template instead (see
https://docs.k3s.io/advanced#configuring-containerd) — this script will not guess
one, because a wrong template takes the node's whole containerd down."
    fi
    dropin_dir="$(dirname "$(echo "$imports_line" | sed -E 's/.*"([^"]+)".*/\1/')")"

    runc_header="$(grep -m1 -E '^\[plugins\..*containerd\.runtimes\.runc\]$' "$CONTAINERD_CONFIG" || true)"
    if [[ -z $runc_header ]]; then
        die "could not find the runc runtime block in $CONTAINERD_CONFIG to model runsc on"
    fi
    runsc_header="${runc_header/runtimes.runc]/runtimes.runsc]}"

    log "registering runsc via $dropin_dir/$DROPIN_NAME"
    mkdir -p "$dropin_dir"
    cat >"$dropin_dir/$DROPIN_NAME" <<EOF
# Managed by deploy/kubernetes/gvisor-k3s.sh — the gVisor runtime handler for
# ProteOS machine pods. Merged into k3s's generated containerd config through the
# imports glob; k3s does not overwrite this file.
$runsc_header
  runtime_type = "io.containerd.runsc.v1"
EOF
}

# --- 2. containerd ----------------------------------------------------------
# k3s rewrites its containerd config from a template on every start and detects
# some runtimes on PATH by itself, so the install order matters: runsc first,
# restart second. Auto-detection is not guaranteed for every release/runtime
# combination, so a drop-in is written when the restart alone does not do it.
UNIT="$(k3s_unit)" || die "neither k3s nor k3s-agent is running on this node"

has_runsc() { grep -rq 'runsc' "$CONTAINERD_CONFIG" "$(dirname "$CONTAINERD_CONFIG")"/*.d/ 2>/dev/null; }

if has_runsc; then
    ok "k3s containerd already has a runsc handler"
elif [[ $CHECK_ONLY -eq 1 ]]; then
    die "k3s containerd has no runsc handler (re-run without --check to register it)"
else
    log "restarting $UNIT so it re-detects the runtime"
    systemctl restart "$UNIT"
    for _ in $(seq 1 30); do
        has_runsc && break
        sleep 2
    done
    if ! has_runsc; then
        log "k3s did not pick the runtime up on its own; registering it explicitly"
        write_dropin
        systemctl restart "$UNIT"
    fi
    for _ in $(seq 1 30); do
        has_runsc && break
        sleep 2
    done
    has_runsc || die "runsc is still not registered in containerd after a restart"
    ok "containerd has a runsc handler"
fi

# Wait for the kubelet to come back before anything below asks the API server
# about this node; a restart takes it NotReady for a few seconds.
for _ in $(seq 1 60); do
    systemctl is-active --quiet "$UNIT" && break
    sleep 2
done

# --- 3. the RuntimeClass ----------------------------------------------------
# The object that lets a pod ASK for the handler. Applying it is part of the k3s
# overlay; this only reports, so the script stays a node-level concern. An agent
# node has no API access at all, which is not a problem to report — the check
# simply belongs somewhere else.
if ! command -v kubectl >/dev/null || ! kubectl get nodes >/dev/null 2>&1; then
    log "no cluster access from this node; check the RuntimeClass from wherever you run kubectl:"
    log "    kubectl get runtimeclass gvisor"
elif kubectl get runtimeclass gvisor >/dev/null 2>&1; then
    ok "RuntimeClass/gvisor exists"
else
    log "RuntimeClass/gvisor not found — apply it with:"
    log "    kubectl apply -k deploy/kubernetes/overlays/k3s/"
fi

# --- 4. prove it actually sandboxes -----------------------------------------
# The only assertion that matters: a pod asking for this RuntimeClass really runs
# under a gVisor kernel rather than the node's. A RuntimeClass naming a handler
# the node does not have leaves pods Pending or failing to start, so check from
# inside a sandbox rather than trusting the config.
#
# PINNED TO THIS NODE, which is the whole point: this script installs a runtime on
# ONE node, and a cluster is very likely to be mixed while it is being rolled out.
# An unpinned pod tests whichever node the scheduler picks — on the first run of
# this script against a three-node cluster it landed on the one node without
# runsc and reported failure for a node that was, in fact, fine.
NODE="$(hostname)"
if [[ $CHECK_ONLY -eq 0 ]] && command -v kubectl >/dev/null && kubectl get runtimeclass gvisor >/dev/null 2>&1; then
    log "smoke-testing a sandboxed pod on $NODE"
    kubectl delete pod gvisor-smoke --ignore-not-found >/dev/null 2>&1 || true
    if kubectl run gvisor-smoke --rm -i --restart=Never --image=busybox:1.36 \
        --overrides="{\"spec\":{\"runtimeClassName\":\"gvisor\",\"nodeSelector\":{\"kubernetes.io/hostname\":\"$NODE\"}}}" \
        -- dmesg 2>/dev/null | grep -qi 'gvisor'; then
        ok "a pod with runtimeClassName: gvisor runs under gVisor on $NODE"
    else
        die "the smoke pod did not report a gVisor kernel — machines would run on this node's kernel
(if it stayed Pending, this node's Kubernetes name may not be \"$NODE\")"
    fi
fi

# --- 5. let the scheduler know ----------------------------------------------
# A machine pod may only be scheduled where the runtime actually exists. The
# RuntimeClass carries a nodeSelector for that (see the k3s overlay), so a node
# has to advertise itself — otherwise machines land on a node without runsc and
# fail to start, which reads as a broken machine rather than an un-rolled-out one.
log "label this node so machine pods may be scheduled to it:"
log "    kubectl label node $NODE proteos.io/sandbox=gvisor --overwrite"

ok "this node can run sandboxed machine pods"
