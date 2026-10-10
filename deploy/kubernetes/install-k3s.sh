#!/usr/bin/env bash
# install-k3s.sh — install the whole of ProteOS into an existing k3s cluster:
# the cluster-agent, and the app stack (Postgres, OpenBao, the control plane,
# the web front end) beside it in proteos-system.
#
# It is the README's quickstart plus "The app stack in the cluster", in one
# idempotent run. Re-running it is safe: existing secrets are kept (nothing is
# rotated unless you ask), and every apply is a `kubectl apply`.
#
#   deploy/kubernetes/install-k3s.sh \
#       --base-url http://192.168.2.220 \
#       --guest-image ghcr.io/ipedrazas/proteos-guest:base-1.2.3 \
#       --app-tag sha-abc1234 \
#       --install-gvisor --ssh-user ivan
#
# What it does NOT do, deliberately:
#   * install k3s itself — bring your own cluster (the kubeconfig context must
#     already work);
#   * decide your isolation for you — machine pods are sandboxed with gVisor
#     unless you pass --no-sandbox, and the agent refuses to start when the
#     RuntimeClass is missing;
#   * initialise OpenBao — the control plane starts on its file backend, as the
#     compose stack does, and the script prints the switch-over steps at the end.
#
# Everything it generates (the agent's key pair, the passwords it invents, the
# two site overlays it writes) lands in --site-dir, which is gitignored. That
# directory is the record of this install: keep it, and a re-run reuses it.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"

# ─── defaults ────────────────────────────────────────────────────────────────
BASE_URL=""
GUEST_IMAGE=""
TEMPLATES_FILE=""
APP_TAG="latest"
AGENT_TAG="latest"
MACHINE_DOMAIN=""
EXTRA_SANS=()
GHCR_USER="${GHCR_USER:-}"
GHCR_TOKEN="${GHCR_TOKEN:-}"
SSH_USER="${SSH_USER:-$(id -un)}"
INSTALL_GVISOR=0
SKIP_GVISOR_CHECK=0
NO_SANDBOX=0
AGENT_REPLICAS=""
WEB_SERVICE_TYPE="LoadBalancer"
SITE_DIR="$REPO/deploy/kubernetes/site"
KCONTEXT=""
SKIP_APP_STACK=0
ROTATE_AGENT_SECRET=0
DRY_RUN=0

SYS_NS=proteos-system
MACH_NS=proteos-machines

usage() {
    sed -n '2,27p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
    cat <<'EOF'

Options
  --base-url URL         The address users open (the web LoadBalancer's, or a
                         name pointing at it). Required.
  --guest-image REF      The OCI image machines boot. Immutable tag or digest,
                         never :latest. Required unless --templates-file.
  --templates-file PATH  A machine-template catalog (templates.json) to mount
                         instead of the single synthesized "base" template.
  --app-tag TAG          Tag for proteos-api / proteos-ui (default: latest —
                         pin a commit tag for a deploy you can roll back).
  --agent-tag TAG        Tag for proteos-node-agent (default: latest).
  --machine-domain DOM   Per-machine editor/preview origin (m-<uuid>.<DOM>);
                         needs wildcard DNS + TLS. Default: disabled.
  --san VALUE            Extra SAN for the agent certificate (repeatable).
                         Node IPs, 127.0.0.1 and the Service names are included
                         automatically.
  --ghcr-user USER       GitHub user for the private GHCR packages.
  --ghcr-token TOKEN     PAT with read:packages. Also read from $GHCR_USER /
                         $GHCR_TOKEN.
  --install-gvisor       ssh to every node and run gvisor-k3s.sh (installs
                         runsc, restarts k3s, proves a pod is sandboxed).
  --ssh-user USER        The ssh user for that (default: $(id -un)).
  --skip-gvisor-check    Do not ssh at all; assume runsc is already installed.
  --no-sandbox           EXPLICIT DOWNGRADE: machine pods share the node kernel.
  --agent-replicas N     Cluster-agent replicas (default: the overlay's 2).
  --web-service-type T   LoadBalancer (default), NodePort or ClusterIP.
  --site-dir DIR         Where generated overlays and secrets go
                         (default: deploy/kubernetes/site).
  --context NAME         kubectl context to use (default: the current one).
  --skip-app-stack       Install the cluster-agent only (control plane elsewhere).
  --rotate-agent-secret  Re-issue the agent token and certificate even if the
                         Secret already exists.
  --dry-run              Print what would happen; change nothing.
  -h, --help             This.
EOF
}

# ─── output ──────────────────────────────────────────────────────────────────
step() { printf '\n\e[1;34m==>\e[0m \e[1m%s\e[0m\n' "$*"; }
info() { printf '    %s\n' "$*"; }
ok()   { printf '\e[1;32m [ok]\e[0m %s\n' "$*"; }
warn() { printf '\e[1;33m[warn]\e[0m %s\n' "$*" >&2; }
die()  { printf '\e[1;31m[fail]\e[0m %s\n' "$*" >&2; exit 1; }

# ─── arguments ───────────────────────────────────────────────────────────────
while [[ $# -gt 0 ]]; do
    case "$1" in
        --base-url)            BASE_URL="$2"; shift 2 ;;
        --guest-image)         GUEST_IMAGE="$2"; shift 2 ;;
        --templates-file)      TEMPLATES_FILE="$2"; shift 2 ;;
        --app-tag)             APP_TAG="$2"; shift 2 ;;
        --agent-tag)           AGENT_TAG="$2"; shift 2 ;;
        --machine-domain)      MACHINE_DOMAIN="$2"; shift 2 ;;
        --san)                 EXTRA_SANS+=("$2"); shift 2 ;;
        --ghcr-user)           GHCR_USER="$2"; shift 2 ;;
        --ghcr-token)          GHCR_TOKEN="$2"; shift 2 ;;
        --install-gvisor)      INSTALL_GVISOR=1; shift ;;
        --ssh-user)            SSH_USER="$2"; shift 2 ;;
        --skip-gvisor-check)   SKIP_GVISOR_CHECK=1; shift ;;
        --no-sandbox)          NO_SANDBOX=1; shift ;;
        --agent-replicas)      AGENT_REPLICAS="$2"; shift 2 ;;
        --web-service-type)    WEB_SERVICE_TYPE="$2"; shift 2 ;;
        --site-dir)            SITE_DIR="$2"; shift 2 ;;
        --context)             KCONTEXT="$2"; shift 2 ;;
        --skip-app-stack)      SKIP_APP_STACK=1; shift ;;
        --rotate-agent-secret) ROTATE_AGENT_SECRET=1; shift ;;
        --dry-run)             DRY_RUN=1; shift ;;
        -h|--help)             usage; exit 0 ;;
        *) die "unknown option: $1 (try --help)" ;;
    esac
done

kc() {
    if [[ -n "$KCONTEXT" ]]; then kubectl --context "$KCONTEXT" "$@"; else kubectl "$@"; fi
}
# Everything that writes goes through this, so --dry-run is honest. It takes
# functions as well as commands, which is why the cluster writes below are
# wrapped in one.
run() {
    if [[ $DRY_RUN -eq 1 ]]; then printf '    \e[2m[dry-run] %s\e[0m\n' "$*"; return 0; fi
    "$@"
}
# create-or-update, so a re-run is an apply rather than an "already exists".
ensure_ns() { kc create namespace "$1" --dry-run=client -o yaml | kc apply -f - >/dev/null; }
put_secret() { local ns="$1"; shift; kc -n "$ns" create secret "$@" --dry-run=client -o yaml | kc apply -f - >/dev/null; }

# ─── 1. preflight ────────────────────────────────────────────────────────────
step "Preflight"

[[ -n "$BASE_URL" ]] || die "--base-url is required (the address users open)"
if [[ $SKIP_APP_STACK -eq 0 && -z "$GUEST_IMAGE" && -z "$TEMPLATES_FILE" ]]; then
    die "--guest-image is required (the OCI image machines boot), or --templates-file"
fi
[[ "$GUEST_IMAGE" == *:latest ]] && die "--guest-image must not be :latest — machine pods pull IfNotPresent, so a moving tag means each node keeps booting whatever it cached first (README, 'Image tags')"
[[ -n "$TEMPLATES_FILE" && ! -f "$TEMPLATES_FILE" ]] && die "no such templates file: $TEMPLATES_FILE"
case "$WEB_SERVICE_TYPE" in LoadBalancer|NodePort|ClusterIP) ;; *) die "--web-service-type must be LoadBalancer, NodePort or ClusterIP" ;; esac
if [[ -n "$AGENT_REPLICAS" && ! "$AGENT_REPLICAS" =~ ^[0-9]+$ ]]; then die "--agent-replicas must be a number"; fi

for t in kubectl openssl; do command -v "$t" >/dev/null || die "$t is not installed"; done
kc version --request-timeout=10s >/dev/null 2>&1 || kc cluster-info --request-timeout=10s >/dev/null \
    || die "cannot reach the cluster with this kubeconfig${KCONTEXT:+ (context $KCONTEXT)}"
info "context: $(kc config current-context 2>/dev/null || echo '?')"
info "server:  $(kc config view --minify -o jsonpath='{.clusters[0].cluster.server}' 2>/dev/null)"

# The node addresses go into the agent's certificate: with klipper (stock k3s)
# the LoadBalancer answers on a node's own address, and which node that is
# changes when a pod moves. A cert naming all of them survives that.
# Name and address in one pass, so a node without an InternalIP cannot shift
# every later address onto the wrong node.
NODES=(); NODE_IPS=()
while IFS=' ' read -r n ip; do
    [[ -n "$n" ]] || continue
    NODES+=("$n"); NODE_IPS+=("$ip")
done < <(kc get nodes -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.addresses[?(@.type=="InternalIP")].address}{"\n"}{end}')
[[ ${#NODES[@]} -gt 0 ]] || die "the cluster reports no nodes"
info "nodes:   ${NODES[*]}"
info "node IPs: ${NODE_IPS[*]}"

# local-path is what k3s ships, and it is NOT encrypted. The agent has no
# per-machine LUKS key on this backend, so this is a real gap, not a nit.
if ! kc get storageclass local-path >/dev/null 2>&1; then
    warn "no 'local-path' StorageClass — the k3s overlay names it for every machine volume. Install the local-path provisioner or patch PROTEOS_K8S_STORAGE_CLASS in $SITE_DIR/cluster-agent."
fi
ok "cluster reachable"

# ─── 2. the sandbox ──────────────────────────────────────────────────────────
step "The sandbox (gVisor)"
if [[ $NO_SANDBOX -eq 1 ]]; then
    warn "--no-sandbox: machine pods will run on the NODE'S SHARED KERNEL."
    warn "A machine runs a coding agent with --dangerously-skip-permissions and"
    warn "unauthenticated terminal/editor/preview listeners. On Firecracker the"
    warn "microVM is what makes that acceptable; here nothing does. See SECURITY.md."
elif [[ $SKIP_GVISOR_CHECK -eq 1 ]]; then
    info "--skip-gvisor-check: assuming runsc is installed on every node."
else
    for i in "${!NODES[@]}"; do
        node="${NODES[$i]}"; ip="${NODE_IPS[$i]:-$node}"
        if [[ $INSTALL_GVISOR -eq 1 ]]; then
            info "installing runsc on $node ($ip) over ssh…"
            run ssh -o BatchMode=yes "${SSH_USER}@${ip}" 'sudo -n bash -s' < "$HERE/gvisor-k3s.sh" \
                || die "gvisor-k3s.sh failed on $node. Run it on the node by hand: sudo deploy/kubernetes/gvisor-k3s.sh"
        else
            info "checking runsc on $node ($ip)…"
            if ! ssh -o BatchMode=yes -o ConnectTimeout=10 "${SSH_USER}@${ip}" 'sudo -n bash -s -- --check' < "$HERE/gvisor-k3s.sh" >/dev/null 2>&1; then
                die "gVisor is not installed (or not verifiable) on $node. Re-run with --install-gvisor, or run 'sudo deploy/kubernetes/gvisor-k3s.sh' on the node. --skip-gvisor-check skips this, --no-sandbox gives it up."
            fi
        fi
        ok "$node: runsc present"
    done
fi

# The RuntimeClass carries this nodeSelector, so an unlabelled node is simply
# never given a machine — which is what you want while a rollout is half done.
if [[ $NO_SANDBOX -eq 0 ]]; then
    for node in "${NODES[@]}"; do
        run kc label node "$node" proteos.io/sandbox=gvisor --overwrite >/dev/null
    done
    [[ $DRY_RUN -eq 1 ]] || ok "nodes labelled proteos.io/sandbox=gvisor"
fi

# ─── 3. namespaces and the site directory ────────────────────────────────────
step "Namespaces and $SITE_DIR"
for ns in "$SYS_NS" "$MACH_NS"; do
    run ensure_ns "$ns"
done
ok "namespaces $SYS_NS, $MACH_NS"

run mkdir -p "$SITE_DIR/cluster-agent" "$SITE_DIR/app-stack"
run chmod 700 "$SITE_DIR"

# ─── 4. the agent's identity ─────────────────────────────────────────────────
# The token the control plane presents and the certificate it pins. The control
# plane reads BOTH from this one Secret (app-stack/base/controlplane.yaml), so
# the two sides can never drift the way two .env files do.
step "The cluster-agent's token and certificate"
if kc -n "$SYS_NS" get secret proteos-cluster-agent >/dev/null 2>&1 && [[ $ROTATE_AGENT_SECRET -eq 0 ]]; then
    ok "Secret proteos-cluster-agent exists — kept (--rotate-agent-secret to re-issue)"
elif [[ $DRY_RUN -eq 1 ]]; then
    info "[dry-run] would issue an agent certificate and token into Secret proteos-cluster-agent"
else
    SANS="DNS:proteos-cluster-agent,DNS:proteos-cluster-agent.${SYS_NS},DNS:proteos-cluster-agent.${SYS_NS}.svc,DNS:proteos-cluster-agent.${SYS_NS}.svc.cluster.local,IP:127.0.0.1"
    for ip in "${NODE_IPS[@]}"; do SANS+=",IP:$ip"; done
    for s in ${EXTRA_SANS+"${EXTRA_SANS[@]}"}; do
        if [[ "$s" =~ ^[0-9]+(\.[0-9]+){3}$ ]]; then SANS+=",IP:$s"; else SANS+=",DNS:$s"; fi
    done
    info "SANs: $SANS"
    openssl req -x509 -newkey rsa:4096 -nodes -days 825 \
        -subj "/CN=proteos-cluster-agent" -addext "subjectAltName=${SANS}" \
        -keyout "$SITE_DIR/agent.key" -out "$SITE_DIR/agent.crt" 2>/dev/null
    chmod 600 "$SITE_DIR/agent.key"
    if [[ ! -s "$SITE_DIR/agent-token" || $ROTATE_AGENT_SECRET -eq 1 ]]; then
        openssl rand -hex 32 > "$SITE_DIR/agent-token"
        chmod 600 "$SITE_DIR/agent-token"
    fi
    put_secret "$SYS_NS" generic proteos-cluster-agent \
        --from-file="PROTEOS_AGENT_TOKEN=$SITE_DIR/agent-token" \
        --from-file="tls.crt=$SITE_DIR/agent.crt" \
        --from-file="tls.key=$SITE_DIR/agent.key"
    ok "Secret proteos-cluster-agent written (material in $SITE_DIR)"
fi

# ─── 5. the pull secret, in BOTH namespaces ──────────────────────────────────
# A pull secret is namespaced and a machine pod does not live where the agent
# does. Both are named `ghcr`, which is what the k3s overlay references.
step "The GHCR pull secret"
if [[ -n "$GHCR_USER" && -n "$GHCR_TOKEN" ]]; then
    for ns in "$SYS_NS" "$MACH_NS"; do
        run put_secret "$ns" docker-registry ghcr \
            --docker-server=ghcr.io --docker-username="$GHCR_USER" --docker-password="$GHCR_TOKEN"
    done
    ok "secret 'ghcr' in $SYS_NS and $MACH_NS"
elif kc -n "$SYS_NS" get secret ghcr >/dev/null 2>&1; then
    ok "secret 'ghcr' already present — kept"
else
    warn "no --ghcr-user/--ghcr-token: ProteOS's GHCR packages are private, so pods will fall back to an anonymous pull and may sit in ImagePullBackOff."
fi

# ─── 6. the cluster-agent overlay ────────────────────────────────────────────
# A thin overlay on deploy/kubernetes/overlays/k3s, so everything security
# relevant (RBAC, the NetworkPolicies, the RuntimeClass) still comes from the
# tracked base and only this site's values live here.
step "Generating $SITE_DIR/cluster-agent"
if [[ $DRY_RUN -eq 0 ]]; then
    {
        cat <<EOF
# Generated by deploy/kubernetes/install-k3s.sh — this site's cluster-agent.
# Edit and re-apply with: kubectl apply -k $SITE_DIR/cluster-agent
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization

resources:
  - ../../overlays/k3s

images:
  - name: ghcr.io/ipedrazas/proteos-node-agent
    newTag: ${AGENT_TAG}
EOF
        if [[ -n "$AGENT_REPLICAS" || $NO_SANDBOX -eq 1 ]]; then
            printf '\npatches:\n'
        fi
        if [[ -n "$AGENT_REPLICAS" ]]; then
            cat <<EOF
  - target: {kind: Deployment, name: proteos-cluster-agent}
    patch: |
      - op: replace
        path: /spec/replicas
        value: ${AGENT_REPLICAS}
EOF
        fi
        if [[ $NO_SANDBOX -eq 1 ]]; then
            # A strategic merge, keyed on the env var's name, so the value is
            # REPLACED rather than a second entry appended after the base's.
            cat <<'EOF'
  - target: {kind: Deployment, name: proteos-cluster-agent}
    patch: |
      apiVersion: apps/v1
      kind: Deployment
      metadata:
        name: proteos-cluster-agent
      spec:
        template:
          spec:
            containers:
              - name: node-agent
                env:
                  # EXPLICIT DOWNGRADE: machine pods share the node's kernel.
                  - name: PROTEOS_K8S_RUNTIME_CLASS
                    value: ""
EOF
        fi
    } > "$SITE_DIR/cluster-agent/kustomization.yaml"
fi
ok "$SITE_DIR/cluster-agent/kustomization.yaml"

step "Applying the cluster-agent"
run kc apply -k "$SITE_DIR/cluster-agent"
if [[ $DRY_RUN -eq 0 ]]; then
    # The agent proves its own preconditions at startup: it refuses to run
    # without the RuntimeClass, and it refuses to run on a CNI that does not
    # enforce NetworkPolicy (it creates a canary pod and tries to reach it
    # through a deny-all policy). A rollout that never completes is usually one
    # of those two — the log says which.
    if ! kc -n "$SYS_NS" rollout status deploy/proteos-cluster-agent --timeout=5m; then
        warn "the cluster-agent did not become ready. Its own log names the cause:"
        kc -n "$SYS_NS" logs deploy/proteos-cluster-agent --tail=40 || true
        die "cluster-agent rollout failed"
    fi
    if kc -n "$SYS_NS" logs deploy/proteos-cluster-agent --tail=200 2>/dev/null | grep -q "NetworkPolicy enforcement verified"; then
        ok "NetworkPolicy enforcement verified by the agent's own canary"
    else
        warn "the agent did not log 'NetworkPolicy enforcement verified' — if PROTEOS_K8S_SKIP_NETPOL_PROBE is set, verify by hand (README, step 3)."
    fi
fi
[[ $DRY_RUN -eq 1 ]] || ok "cluster-agent running"

if [[ $SKIP_APP_STACK -eq 1 ]]; then
    step "Done (cluster-agent only)"
    cat <<EOF
Register this host with your external control plane. Its Service address:

    kubectl -n $SYS_NS get svc proteos-cluster-agent    # EXTERNAL-IP:9090

    PROTEOS_HOSTS='[{"name":"k8s","url":"https://<address>:9090/","driver":"kubernetes"}]'
    PROTEOS_AGENT_TOKEN  = $SITE_DIR/agent-token
    PROTEOS_NODE_CA_FILE = $SITE_DIR/agent.crt
EOF
    exit 0
fi

# ─── 7. the app stack's secret ───────────────────────────────────────────────
# The database password and the state signing key are invented here and kept in
# the site directory; the IdP and GitHub App credentials are read from the
# environment if they are set. Without them the control plane still serves
# /healthz and 401s the API — exactly as the compose stack does.
step "The app stack's secret"
if kc -n "$SYS_NS" get secret proteos-controlplane >/dev/null 2>&1; then
    ok "Secret proteos-controlplane exists — kept (edit it with kubectl to add IdP/GitHub credentials)"
elif [[ $DRY_RUN -eq 1 ]]; then
    info "[dry-run] would write Secret proteos-controlplane (database password, state key${ZITADEL_DOMAIN:+, IdP}${GITHUB_APP_CLIENT_ID:+, GitHub App})"
else
    [[ -s "$SITE_DIR/postgres-password" ]] || { openssl rand -hex 16 > "$SITE_DIR/postgres-password"; chmod 600 "$SITE_DIR/postgres-password"; }
    [[ -s "$SITE_DIR/state-key" ]]        || { openssl rand -hex 32 > "$SITE_DIR/state-key";        chmod 600 "$SITE_DIR/state-key"; }
    cp_args=(--from-file="POSTGRES_PASSWORD=$SITE_DIR/postgres-password"
             --from-file="PROTEOS_STATE_KEY=$SITE_DIR/state-key")
    for v in ZITADEL_DOMAIN ZITADEL_CLIENT_ID GITHUB_APP_CLIENT_ID GITHUB_APP_CLIENT_SECRET; do
        if [[ -n "${!v:-}" ]]; then cp_args+=(--from-literal="$v=${!v}"); fi
    done
    put_secret "$SYS_NS" generic proteos-controlplane "${cp_args[@]}"
    ok "Secret proteos-controlplane written (passwords in $SITE_DIR)"
fi

# ─── 8. the app stack overlay ────────────────────────────────────────────────
step "Generating $SITE_DIR/app-stack"
COOKIE_SECURE=false
[[ "$BASE_URL" == https://* ]] && COOKIE_SECURE=true
if [[ $DRY_RUN -eq 0 ]]; then
    {
        cat <<EOF
# Generated by deploy/kubernetes/install-k3s.sh — this site's app stack.
# Edit and re-apply with: kubectl apply -k $SITE_DIR/app-stack
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization

resources:
  - ../../app-stack/base

configMapGenerator:
  - name: proteos-controlplane
    behavior: merge
    literals:
      - PROTEOS_BASE_URL=${BASE_URL}
      - PROTEOS_COOKIE_SECURE=${COOKIE_SECURE}
      - PROTEOS_IMAGE_REF=${GUEST_IMAGE}
      - PROTEOS_MACHINE_DOMAIN=${MACHINE_DOMAIN}
EOF
        if [[ -n "$TEMPLATES_FILE" ]]; then cat <<EOF
      - PROTEOS_TEMPLATES_FILE=/etc/proteos/templates.json
  - name: proteos-templates
    behavior: replace
    files:
      - templates.json
EOF
        fi
        cat <<EOF

images:
  - name: ghcr.io/ipedrazas/proteos-api
    newTag: ${APP_TAG}
  - name: ghcr.io/ipedrazas/proteos-ui
    newTag: ${APP_TAG}

patches:
  - target: {kind: Service, name: proteos-web}
    patch: |
      - op: replace
        path: /spec/type
        value: ${WEB_SERVICE_TYPE}
EOF
    } > "$SITE_DIR/app-stack/kustomization.yaml"
    if [[ -n "$TEMPLATES_FILE" ]]; then cp "$TEMPLATES_FILE" "$SITE_DIR/app-stack/templates.json"; fi
fi
ok "$SITE_DIR/app-stack/kustomization.yaml"

if [[ "$APP_TAG" == "latest" ]]; then
    warn "--app-tag latest: a deploy of 'latest' is not reproducible and cannot be rolled back by name (DEPLOYMENT.md). Pin a commit tag."
fi

step "Applying the app stack"
run kc apply -k "$SITE_DIR/app-stack"
if [[ $DRY_RUN -eq 0 ]]; then
    # The control plane restarts once or twice here while Postgres comes up —
    # the kubelet's backoff is what compose's depends_on was. Expected.
    for d in proteos-postgres proteos-openbao proteos-controlplane proteos-web; do
        if ! kc -n "$SYS_NS" rollout status "deploy/$d" --timeout=6m; then
            warn "$d did not become ready:"
            kc -n "$SYS_NS" logs "deploy/$d" --tail=40 || true
            die "app stack rollout failed at $d"
        fi
    done
fi
[[ $DRY_RUN -eq 1 ]] || ok "app stack running"

# ─── 9. where it is, and what is left ────────────────────────────────────────
step "Done"
if [[ $DRY_RUN -eq 0 ]]; then
    WEB_ADDR="$(kc -n "$SYS_NS" get svc proteos-web -o jsonpath='{.status.loadBalancer.ingress[0].ip}{.status.loadBalancer.ingress[0].hostname}' 2>/dev/null || true)"
    if [[ -n "$WEB_ADDR" ]]; then info "web Service: $WEB_ADDR (PROTEOS_BASE_URL is $BASE_URL — they must agree)"; fi
    kc -n "$SYS_NS" get svc proteos-web proteos-cluster-agent 2>/dev/null || true
fi

cat <<EOF

Left to do, in the order they matter:

1. OpenBao. The control plane is running on its FILE secrets backend, as the
   compose stack does until OpenBao is initialised. To switch:

     kubectl -n $SYS_NS port-forward svc/proteos-openbao 8200:8200 &
     BAO_ADDR=http://127.0.0.1:8200 deploy/app-stack/openbao-init.sh
     kubectl -n $SYS_NS create secret generic proteos-openbao-unseal \\
       --from-file=unseal-key=deploy/app-stack/bao-unseal-key
     kubectl -n $SYS_NS create secret generic proteos-openbao-approle \\
       --from-file=secret-id=deploy/app-stack/openbao-secret-id
     # then in $SITE_DIR/app-stack/kustomization.yaml add to the ConfigMap merge:
     #   - PROTEOS_SECRETS_BACKEND=openbao
     #   - PROTEOS_OPENBAO_ROLE_ID=<role_id from openbao-init.sh>
     kubectl apply -k $SITE_DIR/app-stack

2. Auth. If ZITADEL_* / GITHUB_APP_* were not in the environment for this run,
   the API 401s every request. Add them to the Secret and restart:

     kubectl -n $SYS_NS edit secret proteos-controlplane
     kubectl -n $SYS_NS rollout restart deploy/proteos-controlplane

3. TLS and the editor origins. Put an Ingress (or your load balancer) that
   terminates TLS in front of proteos-web, and give the per-machine origins
   (m-<uuid>.<domain>) the wildcard record and certificate from DEPLOYMENT.md —
   then set --machine-domain and re-run.

4. Verify the two things that do not fail loudly:

     sudo deploy/kubernetes/gvisor-k3s.sh --check          # on each node
     kubectl -n $MACH_NS run probe --image=curlimages/curl --restart=Never -- \\
       sh -c 'curl -m 3 -s http://<machine-pod-ip>:1024/healthz; echo exit=\$?'
     # a timeout is the pass; a 200 means your CNI is not enforcing policy

5. Storage. k3s's local-path is node-local and NOT encrypted, and this backend
   has no per-machine LUKS key — encrypt the disk under each node, or point
   PROTEOS_K8S_STORAGE_CLASS at a class that does it for you.

Generated material lives in $SITE_DIR (gitignored): the agent key pair and
token, the database password, the state key, and the two overlays. Back it up.
EOF
