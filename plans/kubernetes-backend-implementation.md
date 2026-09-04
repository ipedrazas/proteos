# Plan: Kubernetes backend — run machines as pods instead of Firecracker microVMs

> Source: request to "delegate the execution of a remote agent to a Kubernetes
> cluster" (2026-09-04). Feature work off the master-plan numbering, like
> `port-preview` and `multi-machine`; stages labelled **K0–K6**.
> Status: **feasibility analysis + draft plan. Nothing implemented yet.**

## TL;DR

**Feasible, and cheaper than it looks.** The codebase already has the seam this
needs: the node-agent's `driver.Driver` + `driver.GuestDialer` interface, a
byte-pipe guest tunnel that never parses payloads, and a guest-agent that already
runs without vsock, without a block device and without root-only mounts (that is
exactly how the `dev` driver runs it on a Mac today). The control plane never
reads `vm_handle`, only ever sees `DialGuest(machineID, port) → net.Conn`, and
already tolerates "cold stop, no snapshot".

The recommended shape is a **third node-agent driver, `kubernetes`**, deployed
*inside* the cluster as a "cluster-agent" Deployment. To the control plane it is
one more `hosts` row speaking the unchanged `nodeagent/api` contract. The control
plane needs only small, additive changes (a container image ref on templates, a
driver kind on hosts, poweroff instead of hibernate for that kind).

**The hard part is not code, it is the security model.** Every "no auth needed"
decision in the guest (`--dangerously-skip-permissions`, code-server `--auth none`,
unauthenticated terminal/ssh/preview listeners, NOPASSWD sudo) rests on "the
microVM is the sandbox and vsock is unreachable". A pod on a shared kernel with
TCP listeners invalidates all of those premises at once. The plan therefore makes
a sandboxed `RuntimeClass` (gVisor or Kata) plus enforced Kubernetes
`NetworkPolicy` **preconditions** of the backend, not optional hardening.

Rough effort (one engineer familiar with the repo):

| Stage | Scope | Effort |
| --- | --- | --- |
| K0 | Guest OCI image + guest-agent runs in `docker run` over TCP | 2–3 days |
| K1 | `kubernetes` driver behind `driver.Driver` + contract tests | 1–1.5 weeks |
| K2 | Node-agent image, in-cluster manifests, RBAC, NetworkPolicy, kind e2e in CI | ~1 week |
| K3 | Control-plane additive changes (image_ref, hosts.driver, stop mode, UI) | 3–5 days |
| K4 | SSH sidecar, previews, code-server, docs/runbook | 3–5 days |
| K5 | Isolation hardening: RuntimeClass, pod security, volume encryption option | 1–2 weeks |
| K6 | Optional: FQDN egress, CP-in-cluster, autoscaling | later |

A **tracer bullet** (K0 + minimal K1 + minimal K2: create a machine, get a
terminal, run a task, destroy) is ~2 weeks. Production quality with the
security work is **5–7 engineer-weeks**.

---

## Context: what the survey found

### The abstraction holds (good news)

- **Driver seam.** `nodeagent/internal/driver/driver.go` defines `Driver`
  (`EnsureRunning/Stop/Status/Destroy/List/Reattach/Capacity`) and `GuestDialer`
  (`DialGuest(ctx, machineID, port) → net.Conn`). `buildDriver` in
  `nodeagent/cmd/nodeagent/main.go:115-125` is a runtime switch on
  `PROTEOS_AGENT_DRIVER` (`dev` | `firecracker`). A third case is the whole wiring.
- **The guest tunnel is a dumb pipe.** `nodeagent/internal/httpapi/guest.go`
  does an HTTP `Upgrade: proteos-guest`, hijacks, calls `DialGuest`, then
  `io.Copy` both ways. It never parses what flows. Gateway ↔ guest speak
  WebSocket/HTTP *across* it. A pod driver only has to return a `net.Conn`.
- **The guest-agent already runs "containerised".** `guestagent/internal/listen`
  supports `vsock:` / `unix:` / `tcp:` (`listen.go:26-55`). `persist` has a
  *dir mode* (`PROTEOS_GUEST_PERSIST=<dir>`) with no mount, no fsck, no
  `/dev/vdb`. `runas` degrades cleanly if the run-as user is absent or root.
  `PUT /resume`'s `clock_settime` / `RNDADDENTROPY` already warn-and-continue on
  EPERM. The dev driver drives the *real* guest-agent this way today
  (`nodeagent/internal/driver/dev/dev.go:267-318`).
- **Control plane is backend-neutral above `nodeclient`.** `vm_handle` is
  written and never read. `guest_ip` is displayed, never routed on. The state
  machine, transition audit, broker, poller, injector, guestctl and gateway know
  only machine ids and `DialGuest`. `toStopped` already handles
  `snapshot.present=false` (deletes the row, `poller.go:261-276`), and `boot`
  simply stays `cold`.
- **Hosts are just rows.** `hosts(id, name, agent_url, status)` seeded from
  `PROTEOS_HOSTS`; `nodeclient.Registry` caches a client per host. A cluster
  agent registers as one host. With a single active host the scheduler
  short-circuits without even calling `Capacity` (`scheduler.go:33-36`).

### What is Firecracker-shaped and must be reinterpreted

From the node-agent side (`Driver` obligations a pod cannot honour natively):

1. **`Boot: cold|resumed` and `Snapshot{...FCVersion, MemBytes}`.** Pods have no
   RAM restore. Report `cold` + `Present=false` always. The CP handles it.
2. **`StopModeHibernate`.** Collapses to "delete pod, keep PVC". The
   `hibernating` state becomes cosmetic (poller treats `stopping`/`hibernating`
   as one bucket, `poller.go:247-255`).
3. **`VolumeKey`.** Today a host-side LUKS key that never reaches the guest
   (`volume.go:186-202`). No host in Kubernetes to `luksOpen` on. Either ignore
   it (rely on an encrypted StorageClass; **document the regression**) or feed it
   to a LUKS/dm-crypt init container over a `volumeMode: Block` PVC (K5 option).
4. **`NetworkPolicy{Mode, Domains}`.** Enforced today as host nftables on the tap
   with domains resolved once at boot into IP sets (`network.go:282-344`).
   Kubernetes `NetworkPolicy` is IP/label-based too, so the same
   resolve-to-`ipBlock` snapshot gives parity. FQDN egress needs a CNI extension
   (Cilium/Calico) — later.
5. **`GuestIP` / tap / MAC allocation** via `state.Store.Reserve`. Meaningless;
   the pod driver bypasses the local store entirely and uses pod IP.
6. **`Reattach` = probe a PID.** Becomes "list pods by label
   `proteos.io/machine-id`". The API server is the source of truth; no local
   `state.json`.
7. **`Capacity`.** Synthesize `Σ allocatable − Σ requests` over schedulable nodes
   (or a namespace `ResourceQuota`); `Used*` = Σ requests of our pods. Only
   consulted when >1 host is active.
8. **`DialGuest`.** Direct TCP dial to `podIP:1024..1027` from inside the
   cluster. Keep the `"<port>\n"` preview preamble on 1026 — it is a guest-agent
   protocol contract (`previewfwd.go:68-98`), not a vsock artefact.
9. **`kernel_ref`.** Dead weight. `rootfs_ref` becomes (or is joined by) an OCI
   image ref.

From the guest side (`guestagent/` changes are tiny; the image is the work):

- **No Dockerfile exists for the guest.** `image/build-rootfs.sh` (~1650 lines)
  loop-mounts a Firecracker-CI Ubuntu ext4 and `chroot`s into it. A Dockerfile
  reproduces the *content* far more simply, but the provenance features
  (pinned Claude sha256, `manifest.lock`, `/etc/proteos-release`) must be carried
  over, and the six `verify-*-rootfs.sh` scripts assume a loop-mountable ext4.
- **systemd is asserted** (`build-rootfs.sh:1345-1349`). In a pod the guest-agent
  is PID 1 (it already handles SIGTERM with a 5s grace). **sshd loses its
  supervisor** — `sshfwd` only dials `127.0.0.1:22`, it does not spawn sshd.
- **Workspace root.** Dir mode puts the workspace at `<persist>/workspace` and
  exports `PROTEOS_WORKSPACE`, but `server.go:388` falls back to the hard-coded
  `guestwire.WorkspaceRoot` (`/workspace`) for `/download`. One-line fix or a
  symlink in the image.
- **`/run/proteos/env` must be tmpfs** (secrets package doc). An
  `emptyDir{medium: Memory}` at `/run/proteos` is exactly that.

From the control-plane side (real leaks, all small):

- `chooseHost` is a bin-packer over fixed hosts; a cluster is one "host" whose
  capacity is fuzzy. Acceptable for v1.
- `EnsureRequest` carries `kernel_ref` + `volume_key_b64` + `disk_id/disk_mib`.
  Additive `image_ref` needed; `template.go:127-129` rejects an empty
  `kernel_ref` and must relax for k8s templates.
- `Service.Stop` sends `StopModeHibernate` unconditionally (`service.go:443`).
- `hosts` has no `driver`/kind column, so the CP cannot tell a user why hibernate
  is unavailable, nor stop the scheduler placing an ext4-only template on a
  cluster host.
- `sessionexport.go` writes to the CP's local filesystem — irrelevant to this
  backend unless the CP itself moves into the cluster (K6).

### Deployment and build facts that shape the plan

- The node-agent module has **no external deps and no `go.sum`**; CI and
  `controlplane.Dockerfile` (`COPY nodeagent/go.mod` only) rely on that. Adding
  `client-go` breaks those assumptions in three places (CI cache key, Dockerfile
  copies, Ansible `GOWORK=off` build).
- There is **no node-agent Dockerfile**; `deploy/node-agent/run-node-agent.sh`
  says outright it cannot be containerised (tap/nft/jailer/`/dev/kvm`). That
  assumption is precisely what this backend changes.
- No kind/k3s/minikube/kubectl/helm anywhere in the repo. The KVM CI job is the
  only non-`ubuntu-latest` job.
- The CP e2e harness (`controlplane/internal/httpapi/gateway_e2e_test.go`
  `startNodeAgent(..., extraEnv...)`) and the node-agent contract tests
  (`nodeagent/internal/httpapi/httpapi_test.go`) are driver-agnostic and are the
  natural conformance suite for the new driver.

---

## Architectural decisions

1. **Third driver in the node-agent, not a CP-side abstraction.** The
   cluster-agent is `proteos-node-agent` with `PROTEOS_AGENT_DRIVER=kubernetes`,
   running as a Deployment in the target cluster. The CP registers it as a host
   (`PROTEOS_HOSTS=[{"name":"k8s-eu","url":"https://cluster-agent.../"}]`).
   *Why:* zero change to the guest tunnel, gateway, injector, guestctl, poller,
   broker or SPA. A CP-side `Driver` interface only pays off if we later want the
   CP to schedule *within* clusters or hold per-cluster credentials; revisit then.
2. **Use `client-go`.** Hand-rolling the API (the repo's precedent with
   `wsclient`) is tempting but the surface here — pods, PVCs, NetworkPolicies,
   nodes, watches, in-cluster auth, possibly `pods/portforward` — is too broad to
   maintain by hand. Accept the dependency in `nodeagent/go.mod`; add a `go.sum`;
   update the three places that assume none. No build tag needed (client-go is
   cross-platform), so the default `na:build` still compiles everywhere and unit
   tests use `k8s.io/client-go/kubernetes/fake`.
3. **Direct pod-IP dial for `DialGuest`.** The cluster-agent runs in-cluster, so
   `net.Dial("tcp", podIP:port)` works and avoids SPDY port-forward. A
   `NetworkPolicy` restricts ingress on 1024–1027 to the cluster-agent pod only.
4. **One Pod + one PVC per machine, driver-managed.** No StatefulSet/Deployment
   wrapper: the node-agent already *is* the reconciler (idempotent `Ensure`,
   poller-driven status), and a bare Pod gives exact control over lifecycle and
   `Status` mapping. Pod named `m-<id8>`, PVC `m-<id8>-persist`, all labelled
   `app.kubernetes.io/managed-by=proteos`, `proteos.io/machine-id=<uuid>`.
   Spec is stored as pod annotations so `Reattach`/`List` need no local state.
5. **Stop = delete pod, keep PVC. Destroy = delete both.** Report
   `Boot=cold`, `Snapshot.Present=false`. Hibernate is accepted and treated as
   poweroff until K3 teaches the CP to send `poweroff` for `driver=kubernetes`.
6. **Sandboxed runtime is a precondition, not an option.** Manifests set
   `runtimeClassName: gvisor` (or `kata`) by default; running on plain runc is
   an explicit, documented downgrade (`PROTEOS_K8S_RUNTIME_CLASS=""`), and the
   operator docs say what is lost. Pod security: run as root *inside the sandbox*
   with `capabilities.drop: [ALL]`, `add: [SETUID, SETGID, CHOWN, FOWNER,
   DAC_OVERRIDE]`, `allowPrivilegeEscalation: false`, seccomp `RuntimeDefault`,
   so the guest keeps its root-agent / non-root-`dev` split.
7. **Network policy parity by IP snapshot.** Every machine pod gets a
   `NetworkPolicy`: ingress only from the cluster-agent on 1024–1027; egress
   always denies RFC1918 + cluster CIDRs (mirrors the nft `forward` drop), allows
   DNS to kube-dns, and then `allow_all` / `deny_all` / `allow_domains` /
   `deny_domains` mapped exactly as today via resolved `ipBlock`s. The driver
   *must* refuse to start if the CNI does not enforce NetworkPolicy (probe in K2).
8. **Templates gain `image_ref`.** `rootfs_ref` stays the ext4 filename for
   Firecracker hosts; `image_ref` is the OCI ref for Kubernetes hosts; a template
   can carry either or both, and the scheduler only places a machine on hosts
   whose driver the template supports. `kernel_ref` becomes optional when
   `rootfs_ref` is absent.
9. **Volume key: accepted-and-ignored in v1, with a loud log line and a doc
   note.** Encryption at rest becomes the StorageClass's job. K5 offers a
   dm-crypt init-container option for operators who need the key-per-machine
   property back.

### Non-goals (this plan)

- Moving the control plane, Postgres or OpenBao into Kubernetes (K6 sketch only).
- Live hibernate/resume of pods (CRIU-style checkpointing).
- Per-user namespaces or multi-cluster placement policies.
- Replacing the Firecracker backend. Both coexist; a fleet may mix them.

---

## Stages

### K0 — Guest OCI image; guest-agent runs in a plain container

Goal: `docker run` the guest image locally and reach terminal, control channel,
code-server and preview over TCP. Proves the guest side before any cluster.

- **`image/guest.Dockerfile`** (multi-stage). Stage 1 builds the static guest-agent
  (`CGO_ENABLED=0`, `-trimpath`, `-X main.version`) exactly like
  `build-rootfs.sh:1233`. Stage 2 is `ubuntu:24.04` + the *platform layer* from
  `build-rootfs.sh` (git, vim, sudo, gh + wrapper, code-server, Claude Code with
  pinned sha256, Node + provider CLIs, `dev` user uid 1000, `profile.d`
  providers snippet, managed Claude settings, `/etc/proteos-release`). Use
  `ARG TEMPLATE` to layer the language toolchains and produce one tag per
  template: `ghcr.io/tavon-ai/proteos-guest:<template>-<version>`.
  Extract the apt/npm/claude install steps of `build-rootfs.sh` into shell
  fragments both builds can source, so the two images can't drift.
- **Entrypoint**: guest-agent as PID 1, env:
  `PROTEOS_GUEST_LISTEN=tcp:0.0.0.0:1024`, `_WEB_LISTEN=tcp:0.0.0.0:1025`,
  `_PREVIEW_LISTEN=tcp:0.0.0.0:1026`, `_SSH_LISTEN` unset until K4,
  `PROTEOS_GUEST_PERSIST=/persist`, `PROTEOS_GUEST_ENV_DIR=/run/proteos/env`,
  `PROTEOS_CODESERVER_BIN=/usr/local/bin/code-server`.
- **Guest-agent fixes** (small): make the server's workspace root follow the
  persist handle's `PROTEOS_WORKSPACE` (`server.go:98-102, :386-389`) instead of
  the `/workspace` constant; document that `PUT /resume` is a no-op in this mode.
- **Decide NOPASSWD sudo.** Keep it out of the container image (the pod boundary
  is thinner). Track as a product decision.
- **Verify script**: `image/verify-guest-image.sh` — runs the container, curls
  `/healthz` on 1024, opens a terminal WS, checks `claude --version`,
  replacing the loop-mount verifiers for this image family.
- CI: build the image in the `guestagent` job (no push), publish in `publish`.

### K1 — `kubernetes` driver

Package `nodeagent/internal/driver/kubernetes/`. Files mirror the firecracker
driver's split: `kubernetes.go` (Driver), `pod.go` (spec builder), `status.go`
(phase → `agentapi.State` mapping), `network.go` (NetworkPolicy builder +
domain resolution reuse), `guest.go` (`DialGuest`), `capacity.go`.

- **Config** (`nodeagent/internal/config`): `PROTEOS_K8S_NAMESPACE`,
  `PROTEOS_K8S_KUBECONFIG` (empty ⇒ in-cluster), `PROTEOS_K8S_STORAGE_CLASS`,
  `PROTEOS_K8S_RUNTIME_CLASS` (default `gvisor`), `PROTEOS_K8S_IMAGE_PULL_SECRET`,
  `PROTEOS_K8S_NODE_SELECTOR` (JSON), `PROTEOS_K8S_AGENT_POD_LABELS` (for the
  ingress NetworkPolicy peer), `PROTEOS_K8S_CLUSTER_CIDRS` (egress deny list).
- **`EnsureRunning(spec)`**: idempotent. Ensure PVC (size = `Disk.SizeMiB`,
  RWO, storage class); ensure Pod (image from `spec.ImageRef` — see K3 — with
  `RootfsRef` accepted as a fallback for the tracer bullet; requests/limits
  cpu=`Vcpus`, memory=`MemMiB`; volumes: PVC at `/persist`, memory `emptyDir` at
  `/run/proteos`; readiness probe `GET /healthz` on 1024; runtimeClass; security
  context per decision 6); ensure NetworkPolicy. Return handle
  `k8s:<namespace>/<pod>`. If a pod exists in `Failed`/`Succeeded`, delete and
  recreate (that is the "reboot" path).
- **`Status` / `List`**: `Pending` ⇒ `creating`; `Running`+Ready ⇒ `running`;
  `Running`+not-ready ⇒ `creating`; `Failed`/`Unknown` ⇒ `error` with the
  container's last termination reason; no pod + PVC present ⇒ `stopped`; nothing
  ⇒ `ErrUnknownMachine`. `GuestIP` = `pod.Status.PodIP`. `Boot=cold`,
  `Snapshot.Present=false` always. `DiskID` from the PVC annotation.
- **`Stop(mode)`**: both modes delete the pod with a grace period (guest-agent
  handles SIGTERM) and keep the PVC; log at info when hibernate was requested so
  operators can see the downgrade.
- **`Destroy`**: delete pod, NetworkPolicy, PVC; idempotent on NotFound.
- **`Reattach`**: no-op beyond a list-by-label sanity log — the API server is the
  state. Remove the driver's dependency on `state.Store` (constructor takes the
  clientset + config only).
- **`Capacity`**: list nodes matching the selector, sum `allocatable` minus the
  sum of pod requests on those nodes (cache 15s); `Used*` from our labelled pods.
- **`DialGuest`**: look up pod, require Ready, `net.DialTimeout("tcp",
  podIP:port)`; for preview ports dial 1026 and write `agentapi.PreviewPreamble`.
  Errors map to the existing `guest_unreachable` path.
- **Wiring**: third `case "kubernetes"` in `buildDriver`; the `StartOrphanReaper`
  / metrics duck-typing already makes those optional.
- **Tests**: unit tests with the fake clientset for every method + the phase
  mapping table; reuse `nodeagent/internal/httpapi/httpapi_test.go` lifecycle
  tests parameterised over `dev.New` and `kubernetes.New(fakeClient)`. Add a
  tiny `driver/conformance` helper while here so all three drivers share it.
- **Module hygiene**: add `nodeagent/go.sum`; update
  `deploy/app-stack/controlplane.Dockerfile:21`, the CI cache key comment and
  `deploy/ansible/roles/node_agent` (Ansible build needs the sum file / module
  cache). `go.work.sum` picks up the rest.

### K2 — Cluster-agent deployment and CI

- **`deploy/node-agent/node-agent.Dockerfile`**: distroless static, same shape as
  the CP image, entrypoint `proteos-node-agent`. Publish as
  `ghcr.io/tavon-ai/proteos-node-agent`.
- **`deploy/kubernetes/`** — plain manifests first (kustomize), Helm only if the
  value surface grows: Namespace `proteos-machines`; ServiceAccount; Role
  (pods/pvcs/networkpolicies CRUD + pods/status get in the namespace) and a
  ClusterRole (nodes list/get for capacity); Deployment `proteos-cluster-agent`
  (1 replica, env from a Secret holding `PROTEOS_AGENT_TOKEN` and the TLS pair);
  Service ClusterIP :9090; a default-deny NetworkPolicy for the namespace;
  RuntimeClass example for gVisor.
- **Reachability from the CP** (outside the cluster): Ingress or LoadBalancer
  with TLS terminated *by the agent* (the CP pins `PROTEOS_NODE_CA_FILE` and the
  channel carries the volume key), or a Tailscale sidecar matching the current
  `MgmtIfaces` practice. Document both.
- **Preflight** in the driver at startup: RBAC self-check (`SelfSubjectAccessReview`),
  namespace exists, StorageClass exists, RuntimeClass exists (or explicitly
  disabled), and a NetworkPolicy enforcement probe (create a canary pod with a
  deny policy and confirm a connection is refused; skip with a loud warning if
  `PROTEOS_K8S_SKIP_NETPOL_PROBE=1`).
- **CI**: new `kubernetes` job on `ubuntu-latest` using `kind`: load the guest
  and node-agent images, apply the manifests, run the CP e2e tests with
  `startNodeAgent(..., "PROTEOS_AGENT_DRIVER=kubernetes", ...)` pointing at the
  in-kind agent via `kubectl port-forward`. gVisor is not available in kind, so
  this job runs with `RUNTIME_CLASS=""` and is documented as a functional, not
  isolation, test.

### K3 — Control-plane additive changes

- **Migration `000017_hosts_driver`**: `ALTER TABLE hosts ADD COLUMN driver text
  NOT NULL DEFAULT 'firecracker'`; `PROTEOS_HOSTS` entries gain `"driver"`.
  Surface it in the admin console host list.
- **Templates** (`machine/template.go`): add `image_ref`; require at least one of
  `rootfs_ref`/`image_ref`; `kernel_ref` required only when `rootfs_ref` is set.
  `Template.SupportsDriver(d)`.
- **Scheduler**: filter `ListActiveHosts` by `template.SupportsDriver(host.Driver)`
  before the capacity round-trip; `ErrNoCapacity` message says which drivers the
  template supports.
- **`EnsureRequest.image_ref`** (additive, `omitempty`): `buildEnsureRequest`
  fills it from the machine row (new nullable `machines.image_ref` column copied
  at create, like `rootfs_ref`).
- **Stop mode**: `Service.Stop` looks up the host driver; `kubernetes` ⇒
  `running → stopping` + `StopModePoweroff`. Add `nodeclient`/host capability
  helper `SupportsHibernate()` rather than sprinkling string compares.
- **API/UI**: machine summary gains `backend: firecracker|kubernetes`; the SPA
  hides "hibernate" wording and the snapshot badge for kubernetes machines.
- **Tests**: scheduler filtering, template validation matrix, stop-mode choice,
  migration up/down.

### K4 — SSH, previews, code-server, docs

- **SSH**: add a second container `sshd` to the machine pod (same image, command
  `sshd -D -e`, shares the PVC for `~/.ssh/authorized_keys` and the pod netns);
  set `PROTEOS_GUEST_SSH_LISTEN=tcp:0.0.0.0:1027` so `sshfwd` dials
  `127.0.0.1:22` as today. Host keys persist under `/persist` as the ext4 image
  already does.
- **Previews**: already work via 1026 + preamble; verify the machineweb proxy and
  the `PROTEOS_PREVIEW_PORT_MIN|MAX` allowlist end-to-end in the kind job.
- **code-server**: supervised by `webfwd` inside the guest-agent as today; add
  `PROTEOS_CODESERVER_ARGS` defaults for the container path if needed.
- **Docs**: `docs/architecture.md` (backend table), `DEPLOYMENT.md` (new
  topology section), `RUNBOOK.md` (Part K: cluster-agent ops, "why can't I
  hibernate", NetworkPolicy troubleshooting), `SECURITY.md` (isolation
  properties per backend, explicitly listing what is weaker on Kubernetes).

### K5 — Isolation hardening

- **Volume encryption option**: `PROTEOS_K8S_VOLUME_ENCRYPTION=dmcrypt` ⇒ PVC
  `volumeMode: Block`, privileged init container runs `cryptsetup` with the
  volume key (mounted from a short-lived Secret the driver creates per boot and
  deletes after the init container completes), then mounts to `/persist` via a
  shared `emptyDir` mount point. Restores the per-machine key property.
  Requires a runtime that permits the init container (not gVisor) — document.
- **Pod security**: PodSecurity admission `restricted`-compatible profile where
  the RuntimeClass allows; `ResourceQuota` + `LimitRange` per namespace; pod
  anti-affinity or dedicated node pool for machine pods (`PROTEOS_K8S_NODE_SELECTOR`).
- **Guest listeners**: add an optional per-machine shared secret the cluster-agent
  passes at pod creation and presents on every `DialGuest` (a one-line preamble
  the guest-agent validates), so NetworkPolicy is no longer the *only* thing
  standing between another pod and an unauthenticated terminal.
- **Revisit `--dangerously-skip-permissions`** for the Kubernetes backend if
  operators cannot run a sandboxed runtime; make it a template/provider setting.

### K6 — Later

- FQDN-based egress via Cilium/Calico DNS policies (replaces the IP snapshot).
- Control plane + Postgres + OpenBao in-cluster (needs a PVC for
  `sessionexport`, and the CP's single-instance gateway `Registry` is unchanged).
- Cluster-agent HA (leader election is unnecessary since the API server is the
  state, but the CP dials one URL; a 2-replica Deployment behind the Service
  already works because every method is idempotent).
- Autoscaling / scale-to-zero of machine nodes (Karpenter/cluster-autoscaler
  works out of the box once pods carry requests).

---

## Risks and open questions

| # | Risk / question | Mitigation / owner decision |
| --- | --- | --- |
| 1 | **Shared-kernel isolation** — the product's core claim is "not a container". | Decision 6: sandboxed RuntimeClass by default; SECURITY.md states the delta; plain runc is an explicit downgrade. |
| 2 | **Unauthenticated guest listeners on a network** instead of vsock. | NetworkPolicy ingress restriction is mandatory (preflight probe); K5 adds a per-machine secret. |
| 3 | **Encryption-at-rest regression** (volume key ignored). | Documented; StorageClass encryption recommended; K5 dm-crypt option. |
| 4 | **CNI without NetworkPolicy enforcement** (e.g. default flannel) silently gives every pod access to every machine. | Preflight probe refuses to start; docs list supported CNIs. |
| 5 | **client-go dependency** in the deliberately dependency-free node-agent module. | Accepted (decision 2); three build sites updated in K1; `govulncheck` matrix already covers the module. |
| 6 | **Image drift** between ext4 and OCI builds. | Shared install fragments sourced by both `build-rootfs.sh` and the Dockerfile; `manifest.lock` extended with the image digest. |
| 7 | **Hibernate UX** — users of a mixed fleet see two behaviours. | K3 surfaces `backend` in the API/UI and sends poweroff for kubernetes. |
| 8 | **Capacity fuzziness** — cluster free capacity ignores fragmentation. | Acceptable; the scheduler only ranks hosts; Kubernetes places the pod. |
| 9 | **Who runs the cluster?** Managed (EKS/GKE/AKS) vs self-hosted affects RuntimeClass availability (GKE Sandbox = gVisor; EKS needs a custom AMI). | Open question for the operator; the plan is agnostic. |
| 10 | **`sudo` and `--dangerously-skip-permissions` in a pod.** | K0 drops NOPASSWD sudo from the container image; K5 revisits permissions mode. |

## Suggested first PR (tracer bullet)

1. `image/guest.Dockerfile` + `verify-guest-image.sh` (K0).
2. `nodeagent/internal/driver/kubernetes` with Ensure/Status/List/Stop/Destroy/
   DialGuest, fake-clientset tests, and the httpapi lifecycle test parameterised
   over it (K1, minus Capacity/NetworkPolicy polish).
3. `deploy/kubernetes/` manifests + `node-agent.Dockerfile` + a kind job that
   creates a machine, opens a terminal, runs `agent.run` with a stub provider,
   and destroys it (K2 minimal).
4. Temporary: templates file for the kind job uses `rootfs_ref` = image ref and
   `kernel_ref: "none"`, until K3 lands `image_ref`.
