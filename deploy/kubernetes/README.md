# Kubernetes backend — the cluster-agent

These manifests run a ProteOS **node-agent with the kubernetes driver** inside a
cluster (the "cluster-agent"). It creates one Pod + PVC + NetworkPolicy per
machine and speaks the unchanged `nodeagent/api` contract back to the control
plane, which registers it as one more host.

```
control plane ──TLS+bearer──▶ cluster-agent (Deployment, proteos-system)
                                   │  API server: create/delete machine pods
                                   │  TCP 1024–1027: the guest tunnel, pod IP
                                   ▼
                              machine pods (proteos-machines)
```

## Layout

| File | What it is |
| --- | --- |
| `base/namespace.yaml` | `proteos-system` (the agent) and `proteos-machines` (the machines) |
| `base/rbac.yaml` | ServiceAccount + namespaced Role (pods/PVCs/NetworkPolicies) + ClusterRole (nodes, for capacity) |
| `base/deployment.yaml` | The cluster-agent itself |
| `base/service.yaml` | ClusterIP :9090 |
| `base/networkpolicy.yaml` | Default-deny for `proteos-machines`; the agent's own policy |
| `base/runtimeclass-gvisor.yaml` | Example RuntimeClass (**not** applied by default — see below) |
| `base/quota-example.yaml` | Example ResourceQuota + LimitRange for the machine namespace (**not** applied by default; copy into your overlay with your numbers) |
| `base/networkpolicy-cilium.yaml` | **Cilium clusters only**: lets the agent reach the API server (a NetworkPolicy `ipBlock` cannot, on Cilium). Apply it with the base there |
| `overlays/kind/` | The e2e shape: no RuntimeClass, locally-loaded images. Used by `e2e-kind.sh`, not for production |
| `overlays/k3s/` | A self-hosted k3s cluster: LoadBalancer Service, local-path storage, k3s's CIDRs, the gvisor RuntimeClass, two agent replicas |
| `base/pdb.yaml` | PodDisruptionBudget for the cluster-agent, shaped for one replica or two (K6) |
| `app-stack/` | The control plane, Postgres, OpenBao and the web front end **inside the cluster** (K6) — see "The app stack in the cluster" |
| `gvisor-k3s.sh` | Installs runsc on a k3s node and proves a pod really runs sandboxed |

```sh
kubectl apply -k deploy/kubernetes/base/
```

## Testing it end to end

`deploy/kubernetes/e2e-kind.sh` stands up a kind cluster, builds and loads the
node-agent and guest images, applies these manifests (the real ones, through the
kind overlay), and drives a machine through its whole lifecycle against a real
API server:

```sh
deploy/kubernetes/e2e-kind.sh          # create, test, tear down
deploy/kubernetes/e2e-kind.sh --keep   # leave the cluster up to poke at
```

With `--cni=cilium` it builds the cluster on Cilium instead of kind's CNI,
turns the agent's FQDN engine on, and runs the FQDN egress test as well (see
"FQDN egress"); that needs the `cilium` CLI and is not the CI job.

It is a **functional** test, not a sandbox test — kind has no gVisor, so the
overlay disables the RuntimeClass. What it does prove is the part unit tests
cannot: that pods schedule, PVCs bind and survive a stop, the guest tunnel
reaches the pod IP through the authenticated listeners, and the RBAC in
`base/rbac.yaml` is sufficient. Because kind ≥ 0.24 enforces NetworkPolicy, the
agent's startup canary runs for real here and the script asserts its verdict;
that is where CI proves the CNI under test blocks pod-to-machine traffic. It
then stands a control plane up in front of the agent (`controlplane/…/
kind_e2e_test.go`) for the placement path and the preview proxy.

## Before you apply

**1. The secret.** The Deployment reads `proteos-cluster-agent` for the shared
bearer token and the TLS pair the control plane pins. Create it yourself; it is
deliberately not in git:

```sh
kubectl -n proteos-system create secret generic proteos-cluster-agent \
  --from-literal=PROTEOS_AGENT_TOKEN="$(openssl rand -hex 32)" \
  --from-file=tls.crt=agent.crt --from-file=tls.key=agent.key
```

The token must equal the app stack's `PROTEOS_AGENT_TOKEN`, and the control
plane must pin the CA in `PROTEOS_NODE_CA_FILE`. TLS is mandatory on a
non-loopback listener — the channel carries the bearer token (TAV-27).

**2. A sandboxed RuntimeClass.** This is the one setting that decides whether
this backend is safe. Machine pods run an agent with
`--dangerously-skip-permissions` and unauthenticated terminal / code-server /
preview listeners; on Firecracker the microVM is what makes that acceptable.
A pod on a shared kernel is not. `PROTEOS_K8S_RUNTIME_CLASS` therefore defaults
to `gvisor`, and the agent refuses to start if that RuntimeClass is not
installed.

- **GKE**: enable GKE Sandbox on a node pool (gVisor), then set the node
  selector to that pool.
- **EKS/AKS/self-hosted**: install gVisor or Kata on the machine nodes and apply
  a matching RuntimeClass (see `runtimeclass-gvisor.yaml`).
- **Explicit downgrade**: set `PROTEOS_K8S_RUNTIME_CLASS` to the empty string.
  The agent starts, logs loudly, and machines share the node kernel. Understand
  what you are giving up — see `SECURITY.md`.

**3. A CNI that enforces NetworkPolicy.** Every machine's ingress is restricted
to the cluster-agent pod. On a CNI that ignores NetworkPolicy (plain flannel,
some managed defaults) the objects are accepted and silently do nothing.
Calico, Cilium, Antrea, kube-router, GKE Dataplane V2 and kind ≥ 0.24 all
enforce it.

On **Cilium** apply `base/networkpolicy-cilium.yaml` as well, or the agent
never becomes ready: Cilium does not match a NetworkPolicy `ipBlock` against
the cluster's own nodes, so the agent's API-server egress rule grants nothing
there and only a `CiliumNetworkPolicy` with `toEntities: [kube-apiserver]`
can. The symptom is an agent pod that restarts with an empty log, and
`Policy denied … ->host … :6443` in `cilium monitor --type drop`.

The agent checks this itself at startup rather than trusting a list: it creates
a canary pod in `proteos-machines`, reaches it while a policy permits the
connection, rewrites the policy to deny all ingress, and **refuses to start if
it can still connect**. The log line to look for is `NetworkPolicy enforcement
verified`; the refusal names the fix. The canary runs
`registry.k8s.io/e2e-test-images/agnhost` — set `PROTEOS_K8S_NETPOL_PROBE_IMAGE`
on a cluster that cannot pull from there — and takes the machines' node
selector and pull secret. `PROTEOS_K8S_SKIP_NETPOL_PROBE=1` skips the probe for
an operator who has verified enforcement some other way; the agent then starts
with a warning. To check by hand, against a running machine:

```sh
kubectl -n proteos-machines run probe --image=curlimages/curl --restart=Never -- \
  sh -c 'curl -m 3 -s http://<machine-pod-ip>:1024/healthz; echo "exit=$?"'
# Expected: a timeout. A 200 means your CNI is not enforcing policy.
```

**3b. The guest's listeners are authenticated anyway.** Policy is a control the
data path cannot observe, so it is not the only one. Every machine is started
with a random per-boot secret (a Secret named `m-<id>-guest`, referenced by the
pod, never inlined) that the cluster-agent writes as the first line of every
tunnel connection; the guest closes anything that opens differently. This needs
a guest image that understands the preamble — every image built from this
repository does. `PROTEOS_K8S_GUEST_AUTH=0` turns it off for an older image,
and the agent says so at startup. The Role grants `secrets` get/create/update/
delete in the machine namespace for this; there is deliberately no `list`.

**4. An encrypted StorageClass.** The kubernetes backend cannot use the
per-machine LUKS volume key (there is no host to `luksOpen` on), so encryption
at rest is the StorageClass's job. Set `PROTEOS_K8S_STORAGE_CLASS` to an
encrypted one.

**5. `PROTEOS_K8S_CLUSTER_CIDRS`.** Machine egress always denies RFC1918,
link-local (cloud metadata) and loopback, but the pod and service CIDRs are
cluster-specific. Set them, or a machine can reach cluster services on a
non-RFC1918 range.

**6. A ResourceQuota and LimitRange.** The control plane's scheduler places
machines from a capacity figure the agent synthesizes; nothing inside the
cluster caps the namespace unless you do. `base/quota-example.yaml` is the
shape; the numbers are yours. Machine pods and the preflight canary always
declare requests and limits, so a compute quota is safe to apply. The agent
also reads the quota (K6): the capacity it advertises is the sum of the nodes
present, capped by the quota's `limits.cpu`, `limits.memory` and
`requests.storage` when one exists — so the control plane is never told more
than the namespace may use, and disk, which nodes cannot report, gets a real
bound. On an autoscaled node pool see "Autoscaling".

Pod Security admission on `proteos-machines` is `enforce: baseline`, with
`restricted` at audit/warn. It cannot be `restricted`: the guest runs as root
inside the sandbox with a handful of capabilities so sessions can drop to the
`dev` user, which `restricted` forbids regardless of RuntimeClass. If your
cluster's admission configuration exempts a RuntimeClass, gVisor/Kata pods can
be exempted there; that is an API-server setting, not a namespace label.

## Quickstart: a self-hosted k3s cluster

The five steps, in the order they have to happen. Everything here is explained
in the sections that follow.

```sh
# 1. The sandbox, on every node machine pods may run on. Machines are NOT safe
#    without it, and the agent refuses to start when the RuntimeClass is absent.
#    Run it on the node itself; it installs runsc, registers the runtime with
#    k3s's containerd, and proves a pod on THAT node really gets a gVisor kernel.
sudo deploy/kubernetes/gvisor-k3s.sh
#    Then label the node, from wherever you run kubectl. The RuntimeClass carries
#    a matching nodeSelector, so an unlabelled node is simply never given a
#    machine — which is what you want while a rollout is only half done.
kubectl label node <node> proteos.io/sandbox=gvisor

# 2. The agent's identity: the token the control plane presents, and the TLS
#    cert it pins. The SAN must match the address the control plane will dial —
#    the node IP, or a name pointing at it.
openssl req -x509 -newkey rsa:4096 -nodes -days 825   -subj "/CN=proteos-cluster-agent" -addext "subjectAltName=IP:<NODE_IP>"   -keyout agent.key -out agent.crt
kubectl create namespace proteos-system --dry-run=client -o yaml | kubectl apply -f -
kubectl -n proteos-system create secret generic proteos-cluster-agent   --from-literal=PROTEOS_AGENT_TOKEN="$(openssl rand -hex 32)"   --from-file=tls.crt=agent.crt --from-file=tls.key=agent.key

# 3. Pulling the images. ProteOS's GHCR packages are private, so both the
#    cluster-agent and every machine pod need a pull secret.
kubectl -n proteos-system create secret docker-registry ghcr   --docker-server=ghcr.io --docker-username=<github-user> --docker-password=<PAT with read:packages>
kubectl create namespace proteos-machines --dry-run=client -o yaml | kubectl apply -f -
kubectl -n proteos-machines create secret docker-registry ghcr   --docker-server=ghcr.io --docker-username=<github-user> --docker-password=<PAT with read:packages>
# The k3s overlay already references both by the name `ghcr`.

# 4. The cluster-agent.
kubectl apply -k deploy/kubernetes/overlays/k3s/
kubectl -n proteos-system rollout status deploy/proteos-cluster-agent
kubectl -n proteos-system get svc proteos-cluster-agent   # note EXTERNAL-IP:9090

# 5. Register it with the control plane (see "Registering the host"), and give
#    it a template it can boot ("Giving it something to boot").
```

Then verify the two assumptions the security model rests on, because neither
fails loudly: `deploy/kubernetes/gvisor-k3s.sh --check` for the sandbox, and the
NetworkPolicy probe below for the CNI. k3s enforces NetworkPolicy by default
(flannel plus kube-router's policy controller) unless it was started with
`--disable-network-policy` — verified on a v1.36 cluster, where an unrelated pod
was refused a machine's terminal port and reached it the moment the policy was
removed. Run the probe both ways like that: a connection that fails for an
unrelated reason looks exactly like a policy that works.

**The address in the certificate.** The agent terminates TLS and the control
plane pins its certificate, so the SAN has to match whatever the control plane
dials. With klipper (stock k3s) that is a node's own address, and a machine
rebuild changes which node answers; with MetalLB or a cloud provider it is the
LoadBalancer's stable VIP, which is the better thing to put in a certificate.
Check what you actually got before issuing it:

```sh
kubectl -n proteos-system get svc proteos-cluster-agent   # EXTERNAL-IP
```

**What k3s gives you and what it does not.** `local-path` is a working RWO
StorageClass, but it is node-local — a machine's pod is pinned to the node
holding its volume — and it is **not encrypted**. This backend has no
per-machine LUKS key, so encryption at rest is the StorageClass's job: encrypt
the disk under the node (LUKS on the VM's virtual disk, or a full-disk-encrypted
host) or use a storage layer that does it for you.

## Registering the host

Reachability from a control plane outside the cluster is your choice — an
Ingress or LoadBalancer in front of the Service, or a Tailscale sidecar
(matching the `MgmtIfaces` practice on Firecracker hosts). Either way the agent
terminates TLS itself. Then add it to `PROTEOS_HOSTS`:

```json
[{"name": "k8s-eu", "url": "https://cluster-agent.example.com/", "driver": "kubernetes"}]
```

`driver` is what tells the control plane this host boots OCI images and cannot
hibernate; it must match the agent's own `PROTEOS_AGENT_DRIVER`. If the cluster
is the control plane's *only* host, set `PROTEOS_HOST_DRIVER=kubernetes` for the
primary host row instead.

## Image tags

The cluster-agent Deployment tracks `latest` and therefore pulls `Always`.
Machine pods do the opposite: the driver sets `IfNotPresent`, so a guest image is
pulled once per node and every later machine starts from the local copy — which
is most of why a machine boots in seconds rather than minutes.

That makes the template's `image_ref` a promise: **point it at an immutable tag
or a digest**. A moving tag pinned by `IfNotPresent` means a node keeps starting
machines from whatever it cached first, indefinitely and invisibly, while another
node runs something newer. `ghcr.io/tavon-ai/proteos-guest:base-1.2.3` or
`...proteos-guest@sha256:…`, never `:latest`.

## Giving it something to boot

A machine is only placed on a host whose backend its template can boot, so a
cluster host needs templates carrying an OCI `image_ref`:

```json
{"templates": [
  {"id": "base", "label": "Base", "image_ref": "ghcr.io/tavon-ai/proteos-guest:base-1.2.3",
   "defaults": {"vcpus": 2, "mem_mib": 2048, "disk_mib": 10240}}
]}
```

A template may carry both `image_ref` and `rootfs_ref`/`kernel_ref`, in which
case it runs on either backend and the agent uses the ref it can. Without
`PROTEOS_TEMPLATES_FILE`, the synthesized "base" template takes its image from
`PROTEOS_IMAGE_REF`.

The control plane logs a warning at startup for any host no template can be
placed on, and `POST /api/machines` answers `503 no_capacity` naming the
backends the template needs. Neither is a cluster fault — both mean the catalog
and the fleet disagree.

## Two cluster-agent replicas

One replica is enough. Two survive a node drain or a bad node without the
control plane seeing the host go away, and roll with no gap:

```yaml
# in your overlay
patches:
  - target: {kind: Deployment, name: proteos-cluster-agent}
    patch: |
      - op: replace
        path: /spec/replicas
        value: 2
```

The `overlays/k3s` overlay already does this. It works because every driver
method is idempotent and the API server holds all the state — the control
plane dials the one Service and does not care which pod answers. The one fact
a replica used to know alone, "I am in the middle of ensuring this machine",
is stamped on the machine's NetworkPolicy (`proteos.io/ensured-at`) for the
duration of the ensure, so a status poll that lands on the OTHER replica
between the volume and the pod being created says `creating` rather than the
`stopped` the control plane would read as a failed boot. Each replica runs its
own startup NetworkPolicy canary (budget one canary pod per replica in a
`ResourceQuota`), and the PodDisruptionBudget in `base/` (`maxUnavailable: 1`)
lets a drain move them one at a time without ever making a single replica
undrainable.

## FQDN egress

`allow_domains` / `deny_domains` are, on this backend, an **IP snapshot**: the
domains are resolved when the machine is ensured and written into its
NetworkPolicy as `/32` blocks, because Kubernetes NetworkPolicy is IP-based.
A name that moves is not followed; a name behind a rotating pool is only
partly covered; a wildcard cannot be expressed at all. That is the same
limitation the Firecracker driver's nftables sets have.

On a **Cilium** cluster the agent can do better (K6). With

```yaml
- name: PROTEOS_K8S_FQDN_POLICY
  value: cilium
```

every `allow_domains` machine also gets a `CiliumNetworkPolicy` beside its
NetworkPolicy: a DNS rule that turns Cilium's transparent DNS proxy on for the
pod, and one `toFQDNs` entry per domain — `matchName` for a plain name,
`matchPattern` for a `*.` wildcard — so the policy follows whatever the
machine's own lookups return, for as long as the answer's TTL says. The
snapshot NetworkPolicy is still written (Cilium unions every policy that
selects a pod), so nothing is lost if the proxy has not seen a lookup yet.
The agent refuses to start if the CRD is not served, and the Role in `base/`
already grants `ciliumnetworkpolicies` (a rule for an API group the server
does not have grants nothing, so it is harmless elsewhere).

What it does **not** lift: `deny_domains` stays a snapshot on every CNI —
Cilium's deny rules do not accept `toFQDNs`, and the open-source Calico has no
DNS policy at all (it is an Enterprise feature). Turning the engine off later
does not remove the objects it wrote; a machine's leaves with Destroy, and
`kubectl -n proteos-machines delete ciliumnetworkpolicies -l
app.kubernetes.io/managed-by=proteos` removes the rest.

A policy may list a single-level wildcard (`*.githubusercontent.com`): it
matches every subdomain, not the bare name, and only an FQDN engine follows
it — the snapshot backends (Firecracker, or this one without the engine)
skip a wildcard with a warning, so a wildcard-only allow list there leaves a
machine with DNS and nothing else. The e2e uses exactly that to prove the
engine is really following names rather than replaying a snapshot: a
machine allowed `*.github.com` reaches `api.github.com`, and not
`example.com`.

## Autoscaling

Machine pods carry requests and limits, so the cluster autoscaler and
Karpenter both scale a node pool up for them out of the box. Two things the
driver does for the way DOWN:

- Every machine pod carries `cluster-autoscaler.kubernetes.io/safe-to-evict:
  "false"` and `karpenter.sh/do-not-disrupt: "true"`. To an autoscaler a bare
  pod with a PVC is evictable; to ProteOS it is someone's running session, and
  on a node-local StorageClass it cannot come back elsewhere. A node with a
  running machine on it is therefore never consolidated away; a stopped
  machine has no pod and constrains nothing.
- The capacity the agent advertises is, by default, the nodes PRESENT. On a
  pool the autoscaler grows on demand that is what the pool has grown to, not
  what it can hold — and a pool scaled to zero would advertise nothing and
  never be given the machine that makes it grow. Set
  `PROTEOS_K8S_CAPACITY_FROM_QUOTA=1` and apply a `ResourceQuota` bounding
  `limits.cpu`, `limits.memory` and `requests.storage`: the quota then IS the
  advertised capacity, which is what an operator means by "how big may this
  pool get". (This only matters when the control plane has more than one host
  to rank; with a single host it never asks.)

## The app stack in the cluster

`app-stack/` runs the rest of ProteOS — Postgres, OpenBao with its
auto-unsealer, the control plane, and the web front end — in `proteos-system`
beside the cluster-agent (K6). It is `deploy/app-stack/compose.yaml` expressed
as Kubernetes objects with the same images and the same environment, plus the
two things a cluster makes possible: the control plane dials the cluster-agent
by Service name over the pod network, and it reads the agent's token and
certificate **from the agent's own Secret**, so the two can never disagree.
The session export directory (TAV-141) lands on the control plane's data PVC.

```sh
# 1. The cluster-agent first (above) — its Secret is the control plane's too.
# 2. The app stack's secrets. ZITADEL_*/GITHUB_* may be added later; without
#    them the control plane serves /healthz and 401s the API, as compose does.
kubectl -n proteos-system create secret generic proteos-controlplane \
  --from-literal=POSTGRES_PASSWORD="$(openssl rand -hex 16)" \
  --from-literal=PROTEOS_STATE_KEY="$(openssl rand -hex 32)" \
  --from-literal=ZITADEL_DOMAIN=auth.example.com --from-literal=ZITADEL_CLIENT_ID=... \
  --from-literal=GITHUB_APP_CLIENT_ID=... --from-literal=GITHUB_APP_CLIENT_SECRET=...
# 3. Copy app-stack/overlays/k3s, set PROTEOS_BASE_URL and PROTEOS_IMAGE_REF,
#    pin the api/ui images, and apply it.
kubectl apply -k deploy/kubernetes/app-stack/overlays/k3s/
kubectl -n proteos-system get svc proteos-web     # the address for PROTEOS_BASE_URL
```

The control plane pod restarts once or twice on a first apply while Postgres
comes up (compose's `depends_on` is the kubelet's backoff here). OpenBao boots
sealed and unused: the control plane starts on its file backend, exactly as
the compose stack does until `openbao-init.sh` has run. Run it against a
port-forward (`kubectl -n proteos-system port-forward svc/proteos-openbao
8200:8200`), then put the unseal key and the AppRole secret_id where the
manifests look for them and switch the backend:

```sh
kubectl -n proteos-system create secret generic proteos-openbao-unseal --from-file=unseal-key=bao-unseal-key
kubectl -n proteos-system create secret generic proteos-openbao-approle --from-file=secret-id=openbao-secret-id
# then PROTEOS_SECRETS_BACKEND=openbao and PROTEOS_OPENBAO_ROLE_ID in your overlay's ConfigMap, and re-apply
```

Everything else in RUNBOOK Part D applies unchanged. The web Service is a
LoadBalancer in the k3s overlay and a ClusterIP in the base — put an Ingress
that terminates TLS in front of it on a cluster that has one, and remember the
editor/preview origins need the wildcard record and certificate from
DEPLOYMENT.md. Exposure of the *agent* is no longer needed at all when the
control plane is in the cluster; `overlays/k3s`'s LoadBalancer for it is only
for a control plane outside.

`controlplane/internal/httpapi/incluster_e2e_test.go` drives a deployed stack
as a browser would (a seeded session, the JSON API through the web tier, the
terminal WebSocket): `PROTEOS_E2E_CP_URL`, `PROTEOS_E2E_CP_DATABASE_URL` and
`PROTEOS_E2E_CP_ORIGIN` against two port-forwards.

## Known differences from a Firecracker host

| | Firecracker | Kubernetes |
| --- | --- | --- |
| Isolation | microVM (own kernel) | sandboxed runtime, or the node's kernel |
| Hibernate | RAM snapshot, resume | poweroff; the volume is kept, the machine boots cold. Stop moves the machine through `stopping`, not `hibernating`, and the console hides the snapshot field |
| Encryption at rest | per-machine LUKS key | StorageClass |
| Egress policy | nftables on the tap | NetworkPolicy, IP snapshot; on Cilium, `allow_domains` follows names live (`PROTEOS_K8S_FQDN_POLICY=cilium`) |
| Inbound SSH | sshd under systemd | sshd as a sidecar container in the machine pod, on the pod's loopback |
