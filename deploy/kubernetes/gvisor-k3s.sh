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

# --- 2. containerd ----------------------------------------------------------
# k3s rewrites its containerd config from a template on every start and, since
# v1.30, detects runtimes that are already on PATH and registers a handler for
# them. So the install order matters: runsc first, restart second.
if grep -q 'runsc' "$CONTAINERD_CONFIG" 2>/dev/null; then
    ok "k3s containerd already has a runsc handler"
elif [[ $CHECK_ONLY -eq 1 ]]; then
    die "k3s containerd has no runsc handler (restart k3s after installing runsc)"
else
    log "restarting k3s so it re-detects the runtime"
    systemctl restart k3s
    for _ in $(seq 1 30); do
        grep -q 'runsc' "$CONTAINERD_CONFIG" 2>/dev/null && break
        sleep 2
    done
    if ! grep -q 'runsc' "$CONTAINERD_CONFIG" 2>/dev/null; then
        cat >&2 <<'MSG'
Older k3s releases do not auto-detect runtimes. Register one explicitly rather
than letting this script guess a config template for your version: a wrong
containerd template takes the whole node's containerd down, not just gVisor.

  https://docs.k3s.io/advanced#configuring-containerd

Seed a config template from the generated config.toml named above, append a
runsc runtime to it, restart k3s, then re-run this script.
MSG
        die "k3s did not register a runsc runtime after a restart"
    fi
    ok "k3s containerd registered the runsc handler"
fi

# --- 3. the RuntimeClass ----------------------------------------------------
# The object that lets a pod ASK for the handler. Applying it is part of the
# k3s overlay; this only reports, so the script stays a node-level concern.
if kubectl get runtimeclass gvisor >/dev/null 2>&1; then
    ok "RuntimeClass/gvisor exists"
else
    log "RuntimeClass/gvisor not found — apply it with:"
    log "    kubectl apply -k deploy/kubernetes/overlays/k3s/"
fi

# --- 4. prove it actually sandboxes -----------------------------------------
# The only assertion that matters: a pod asking for this RuntimeClass really
# runs under a gVisor kernel rather than the node's. A RuntimeClass naming a
# broken handler leaves pods Pending or, worse, silently falls back — so check
# from inside a sandbox rather than trusting the config.
if [[ $CHECK_ONLY -eq 0 ]] && kubectl get runtimeclass gvisor >/dev/null 2>&1; then
    log "smoke-testing a sandboxed pod"
    kubectl delete pod gvisor-smoke --ignore-not-found >/dev/null 2>&1 || true
    if kubectl run gvisor-smoke --rm -i --restart=Never --image=busybox:1.36 \
        --overrides='{"spec":{"runtimeClassName":"gvisor"}}' -- dmesg 2>/dev/null |
        grep -qi 'gvisor'; then
        ok "a pod with runtimeClassName: gvisor runs under gVisor"
    else
        die "the smoke pod did not report a gVisor kernel — machines would run on the node's kernel"
    fi
fi

ok "this node can run sandboxed machine pods"
