# Plan: Kubernetes backend — run machines as pods instead of Firecracker microVMs

> Source: request to "delegate the execution of a remote agent to a Kubernetes
> cluster" (2026-09-04). Feature work off the master-plan numbering, like
> `port-preview` and `multi-machine`; stages labelled **K0–K6**.
> Status: **K0–K5 implemented** (K5: 2026-09-05). K0–K4 were verified end to
> end on a real multi-node k3s cluster with gVisor and a policy-enforcing CNI —
> see "What a production-shaped cluster found". K5 closed the one K2 promise
> that had been quietly dropped (the agent now PROVES its CNI enforces policy
> before it serves), put a second, CNI-independent control on the guest's
> listeners, and stood a control plane up inside the kind job. The dm-crypt
> option was dropped — see the K5 notes for why. K6 not started. See
> "Implementation notes" at the end for where the build departed from this
> plan, and "What the kind e2e found" for the bugs that only a real API server
> could surface.

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

> Landed 2026-09-04 as migrations `000021_hosts_driver` +
> `000022_machines_image_ref` (17–20 were taken). See the K3 implementation
> notes below.

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

> Landed 2026-09-05. See the K4 implementation notes below.


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

> Landed 2026-09-05. See the K5 implementation notes below. Reordered from the
> original list: the netpol probe first, because K2 had promised it and shipped
> without it, and the guest secret second, because it was the only mitigation
> for risk 2 once the probe turned out not to exist.

- **NetworkPolicy enforcement probe** (the K2 promise): at startup the agent
  creates a canary listener, proves it can reach it while a policy PERMITS the
  connection, rewrites the policy to DENY, and refuses to start if the dial
  still succeeds. `PROTEOS_K8S_SKIP_NETPOL_PROBE=1` skips it, loudly.
- **Guest listeners**: a per-boot shared secret the cluster-agent mints into a
  Secret at pod creation and presents as a one-line preamble on every
  `DialGuest`; the guest-agent gates all four listeners behind it. Default on;
  `PROTEOS_K8S_GUEST_AUTH=0` for an older image.
- **Control plane in the kind job**: a `kind_e2e`-tagged control-plane test
  drives the K3 placement path, poweroff-not-hibernate, and the machine-web
  preview proxy with BOTH port allowlists against a real machine.
- **Pod security**: PodSecurity stays `enforce: baseline` — `restricted` is
  not reachable for a guest that runs as root inside the sandbox, whatever the
  RuntimeClass (a per-RuntimeClass exemption is an API-server admission
  setting, not a namespace label). `ResourceQuota` + `LimitRange` ship as an
  example manifest with operator-chosen numbers.
- ~~**Volume encryption option** (dm-crypt init container)~~ **Dropped.** It
  needs a privileged init container, which gVisor does not permit and
  `baseline` PodSecurity rejects — so the headline feature would have been
  unusable on the recommended configuration and available only on the
  documented downgrade. Encryption at rest on this backend is the
  StorageClass's property, stated as such in SECURITY.md. Revisit only if a
  Kata-only fleet asks for it.
- **`--dangerously-skip-permissions`** for the Kubernetes backend: still a
  product decision, unchanged. The two controls above make the shared-kernel
  downgrade less dangerous but do not make it safe.

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
| 2 | **Unauthenticated guest listeners on a network** instead of vsock. | NetworkPolicy ingress restriction (proved by the K5 preflight probe) AND a per-boot listener secret (K5); either alone would have to hold. |
| 3 | **Encryption-at-rest regression** (volume key ignored). | Documented; StorageClass encryption is the answer. The dm-crypt option was dropped in K5 (needs a privileged init container gVisor forbids). |
| 4 | **CNI without NetworkPolicy enforcement** (e.g. default flannel) silently gives every pod access to every machine. | K5 preflight canary refuses to start (K2 had promised this and shipped without it); docs list supported CNIs. |
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

---

## Implementation notes (K0–K2, 2026-09-04)

What landed, and where it differs from the plan above.

### Done

- **K0** — `image/guest.Dockerfile` (multi-stage, `ARG TEMPLATE` ∈
  base/go/node/python/full), `image/verify-guest-image.sh`, and the guest-agent
  workspace-root fix (`persist.Workspace()` + `server.WithWorkspaceRoot`, also
  applied to code-server's open folder, which the plan did not mention).
- **K1** — `nodeagent/internal/driver/kubernetes` with the full Driver +
  GuestDialer surface, config plumbing, `buildDriver` wiring and a startup
  preflight. Fake-clientset unit tests, NetworkPolicy tests, and the shared
  `driver/conformance` suite run against both dev and kubernetes.
- **K2** — `deploy/node-agent/node-agent.Dockerfile`, `deploy/kubernetes/`
  (kustomize), the CI `guest-image` job, and the publish matrix entries.
  Documentation landed early: SECURITY.md's isolation-model section and the
  architecture backend table, because K1/K2 code and manifests reference them.

### Departures from the plan

1. **Shared image fragments are the FETCH layer, not the install steps**
   (`image/lib/fetch.sh`). `build-rootfs.sh`'s install functions are inseparable
   from their loop-mount context — the extract-only `dpkg-deb` dance exists
   because reinstalling the base closure inside a Firecracker rootfs breaks
   snapshot resume, which is meaningless for a container layer. What must not
   drift is *which* artifact and *how it is verified*, so that is what both
   builds now share (`fetch_claude/node/codeserver/gh/go`).
2. **`image_ref` landed in K1, not K3.** The driver needs it to boot anything;
   it is additive and `omitempty`, so a control plane that predates it still
   produces valid requests. The control-plane half of K3 (the column, the
   template validation, the scheduler filter) is still outstanding.
3. **`driver.ErrBadSpec` is new.** Without it, a template a kubernetes host can
   never boot returned 500 and the poller retried forever. It maps to 400.
4. **The plan's "node-agent has no go.sum" is out of date** — prometheus was
   already a dependency. The real trap was that `nodeagent/go.sum` was
   incomplete for a NON-workspace build, which is how both the container image
   and the Ansible role build the module. It is now tidied standalone under the
   builder toolchain.
5. **RestartPolicy is `Always`, not `Never`.** The kubelet supervises the
   guest-agent the way systemd does in the ext4 image, so a crash restarts the
   container and keeps the pod IP. The cost is that a crash-looping machine
   stays in phase Running, so `phaseToState` checks for CrashLoopBackOff
   explicitly — otherwise a broken machine would report `creating` forever.
6. **Ingress fails closed with no agent pod labels.** The plan did not say what
   happens when `PROTEOS_K8S_AGENT_POD_LABELS` is unset. Writing a rule with an
   empty pod selector would match every pod in the namespace, so the driver
   writes NO ingress rule instead: an unreachable machine is a visible failure,
   an over-permissive policy is not.
7. **The verify script uses a named volume, not a bind mount.** The guest keeps
   a SQLite `machine.db` on the persist root, and Docker Desktop bind mounts do
   not support the POSIX locks SQLite needs — persistence silently degraded to
   ephemeral. Worth knowing before debugging it again.
8. **The NetworkPolicy enforcement probe was NOT built here**, although this
   stage promised it and decision 7 made it mandatory. The preflight checked
   RBAC, the namespace and the RuntimeClass, and left the CNI question to a
   manual curl in the README — so for the whole of K2–K4 the agent started
   cleanly on a CNI that ignores policy, and risk 4 was listed as mitigated
   when it was not. Recorded here because the plan's status header did not say
   so; fixed in K5.

### What the kind e2e found

The job (`deploy/kubernetes/e2e-kind.sh`) was written last and paid for itself
immediately. Every one of these passed unit tests against the fake clientset and
would have shipped:

1. **The cluster-agent could not start at all.** `run()` opened the on-disk
   state.Store before choosing a driver, so it crash-looped against the
   read-only root filesystem its own Deployment sets. The kubernetes driver
   keeps no local state — the code just did not match the design.
2. **No machine could be scheduled on a small cluster.** Requests equalled
   limits for cpu as well as memory, so a 1-vcpu machine demanded a whole free
   core. Fixed with `PROTEOS_K8S_CPU_REQUEST_RATIO` (default 1.0 — production
   behaviour unchanged); memory stays request == limit, because it is
   incompressible.
3. **Terminating objects were reported as present.** A real cluster keeps a pod
   visible for its grace period and pins a PVC with the pvc-protection
   finalizer, so a destroyed machine kept answering 200. The fake clientset
   deletes instantly and has no finalizers, so nothing else could have caught it.
4. **Dir mode never gave the session user its home or workspace.** Disk mode
   chowned them and seeded dotfiles; dir mode only created the directories,
   because the helpers lived in `mount_linux.go`. Harmless while dir mode was a
   dev-only override; fatal once it became this backend's only mode, where the
   agent is root and sessions are not. The shell said `Permission denied` on its
   own workspace.
5. **Dir mode's `$HOME` and session cwd disagreed** — a session started in the
   image's `/home/dev`, which dies with the container, while `$HOME` pointed at
   the volume. Anything written relative to cwd was silently ephemeral.

Plus one design change made on its own merits and confirmed here: the readiness
probe is `exec`, not `httpGet`. The kubelet performs httpGet probes from the
node's address, which would force the machine's NetworkPolicy to admit a second
peer on the very ports it exists to restrict — and CNIs disagree about whether
probe traffic is policed at all, so readiness would have become a property of
the cluster's CNI rather than of the guest.

Two of the early "failures" were the test lying to itself: a PTY echoes what is
typed into it, so a command containing its own completion marker matched the
echo rather than the output. Worth knowing before writing the next one.

## Implementation notes (K3, 2026-09-04)

`hosts.driver` (000021) and `machines.image_ref` (000022), template `image_ref`
+ `Template.SupportsDriver`, the scheduler's driver filter,
poweroff-for-kubernetes in `Service.Stop`, and `backend` in the API, the console
and the admin fleet table. Config: `PROTEOS_HOST_DRIVER`, `"driver"` in
`PROTEOS_HOSTS`, `PROTEOS_IMAGE_REF`.

### Departures from the plan

1. **The driver-kind vocabulary lives in `nodeagent/api`**, next to the stop
   modes and states, not in a control-plane package. Both sides of the fleet
   name these kinds — the agent selects one with `PROTEOS_AGENT_DRIVER`, the
   control plane records which one a host runs — so the wire-contract package is
   where they cannot drift. `DriverSupportsHibernate` and `DriverBootsImage` are
   the capability helpers the plan asked for; they read as questions about a
   backend rather than string compares at each call site.
2. **`resolveCreate` returns a `Template`, not a pair of refs.** Placement now
   depends on the whole template rather than one ref, and
   `ensureOnAgent`/`buildEnsureRequest` read every ref off the machine row
   instead of taking two of them as arguments — all three callers were already
   passing the row's own values.
3. **An empty driver in `UpsertHostByName` means the column default.** The kind
   is validated at config load (`config.validateHostDriver`), so by the time a
   host is seeded a wrong value is already impossible; leaving the query
   permissive keeps every caller that does not care about backends — the tests,
   and any caller predating the column — writing hosts as before, while the
   CHECK constraint still rejects a genuinely unknown kind.
4. **The `rootfs_ref`-as-image fallback in the kubernetes driver is gone.** It
   existed only until `image_ref` had a control plane behind it. Keeping it
   would turn a placement mistake into an `ImagePullBackOff` on an ext4
   filename; `driver.ErrBadSpec` says what is actually wrong.
5. **Startup warns about a host no template can boot.** The plan's failure mode
   was a `503 no_capacity` the first time a user tried to create a machine. The
   catalog and the fleet are both known at startup, so the mismatch is reported
   there too.
6. **`PROTEOS_HOSTS` now reaches the app-stack container.** It was documented in
   `.env.example` but never passed through `compose.yaml`, so registering a
   cluster host by following `deploy/kubernetes/README.md` would have done
   nothing on the shipped stack.

## Implementation notes (K4, 2026-09-05)

The sshd sidecar, code-server verified against a real pod, a k3s overlay with
gVisor, and the operator docs (README quickstart, RUNBOOK Part K, SECURITY.md's
SSH row, the DEPLOYMENT.md topology entry).

### Departures from the plan

1. **The plan's `PROTEOS_GUEST_SSH_LISTEN` line was the easy half.** What it did
   not mention is that three things resolve `~` for the session user and they
   must agree: the guest agent starts sessions in the persisted home, the
   injector writes `authorized_keys` there, and sshd reads `$HOME` and
   `AuthorizedKeysFile` from the PASSWD entry. The ext4 image aligns them with a
   bind mount, which needs a mount namespace this backend does not have, so the
   image sets the `dev` user's passwd home to `/persist/home` instead.
2. **The injector was already writing to the wrong home** (fixed here, in
   guestagent): sessions used the persisted home but the secrets store used the
   OS user's, so on this backend a pushed profile file had been landing in a
   directory that dies with the container and that no session's `$HOME` pointed
   at. Invisible until SSH made it fatal.
3. **`UsePAM yes`, not `no`.** With PAM off, sshd runs its own locked-account
   check and refuses every login — public key included — for an account whose
   shadow password starts with `!`, which is every account `useradd` creates.
   `pam_loginuid` is made optional, for the reason it is optional in every
   container.
4. **Host keys are deleted from the image at build time.** `openssh-server`'s
   postinst generates them; baked in, every machine from that image would share
   one identity and any machine could impersonate any other to a client that
   pinned the fingerprint.
5. **A machine is running when its GUEST is ready, and the ssh dial waits
   separately.** Gating "running" on the whole pod would make a machine with a
   broken sshd entirely unusable — no terminal — for a feature many users never
   touch. Instead `DialGuest` answers the ssh port with "sshd is not ready yet"
   during the window where a first boot is still generating host keys.
6. **`Capacity.Used` now comes from the machines' declared shapes**, not their
   pod requests. A request carries `CPURequestRatio` and the sidecar's slice,
   neither of which the control plane asked for; summing them reported a 1-vcpu
   machine as 2, and then (after the rounding was fixed) as 0 on a cluster
   configured to oversubscribe.

### What the kind e2e found this time

Everything above from 5 onward, plus two tests that were lying to themselves in
new ways — worth recording because the file already carried a warning about the
first kind:

1. **A PTY echo passed for a host key fingerprint.** The command printed a tag it
   also contained, so the parser matched the echo of the awk program: the
   before/after comparison was the command against itself and would have stayed
   green through any host-key regression. This is exactly what `echoMarker`
   exists to prevent, one file over.
2. **The fingerprint could not be read from the volume at all.** The host key
   directory is root-only (private keys live there) and sessions are
   unprivileged. Asking sshd what it presents (`ssh-keyscan`) is both permitted
   and the stronger assertion.
3. **The image verify script probed with `sh`**, whose `/bin/sh` is dash, which
   has no `/dev/tcp` — it reported "sshd never listened" against a healthy
   daemon.

The fake clientset also had to grow the sshd container's status: modelling one
container made every simulated machine look like one whose sshd never came up.
That is the third time this backend's fake has been confidently wrong about
something a real cluster settles immediately.

### What a production-shaped cluster found (2026-09-05)

Run against a three-node k3s v1.36 cluster (containerd 2.3.4, MetalLB, kube-router
policy enforcement, gVisor installed on two nodes), driving the whole `kind_e2e`
suite through the deployed cluster-agent. All six tests pass, and a machine's own
`/proc/version` reports `4.19.0-gvisor` — so this is the first evidence that a
ProteOS machine is actually sandboxed rather than merely asked to be.

The two K5 assumptions, both demonstrated with a control rather than assumed:

- **The sandbox contains the machine.** Both the guest container and the sshd
  sidecar report a gVisor kernel; the toolchain (Claude, node, git, code-server,
  sshd, PTYs) all work inside it.
- **The CNI enforces NetworkPolicy.** An unrelated pod is refused a machine's
  terminal port and reaches it the moment the policy is deleted. Machine egress
  to RFC1918 is refused; egress to the internet works.

Four things the cluster corrected that nothing else could have:

1. **`gvisor-k3s.sh` gave up where it should have acted.** k3s did not
   auto-detect runsc, and the script refused to guess a containerd template —
   right, but there is a supported drop-in mechanism it should have used, reading
   the import directory and plugin table name out of the config k3s generated on
   that node rather than assuming either.
2. **It restarted the wrong unit** (`k3s` on a node running `k3s-agent`) and its
   smoke pod was unpinned, so it tested whichever node the scheduler picked — on
   a half-rolled-out cluster it landed on the one node without runsc and reported
   failure for a node that was fine.
3. **A RuntimeClass needs a nodeSelector.** Without one a machine is scheduled
   anywhere and fails to start on a node without the runtime, which reads as a
   broken machine rather than an un-rolled-out cluster. A cluster is mixed for
   the whole of any rollout.
4. **Machines inherited the cluster's `ndots=5`.** On a cluster whose search
   domain answers wildcards, `github.com` resolved through it to an internal
   address and the machine's own egress policy then blocked it. git, npm and the
   coding agent all fail to reach the internet, on a machine whose NetworkPolicy
   is correct and a cluster whose DNS is working as configured — it reads as a
   ProteOS networking bug. Machine pods now set `ndots=1`.

Two environment facts worth recording for anyone doing this again: a MetalLB VIP
is an L2 announcement that does not cross a subnet boundary (the certificate SAN
has to name whatever the control plane will actually dial), and importing images
into `k3s ctr` is the way to test an unpublished build without putting a registry
credential in the cluster.

### Still open after K4 (resolved in K5 unless noted)
- **CI did not assert isolation.** Of the two assumptions, one is now asserted
  on every run: kind ≥ 0.24 enforces NetworkPolicy, so the agent's K5 canary
  runs for real in the kind job and the script requires its verdict. The other
  — that a sandboxed runtime contains a machine — still needs a runner that
  can host gVisor, and remains verified by hand (k3s, 2026-09-05) only.
- **No control plane drove the cluster in CI.** Now it does
  (`controlplane/internal/httpapi/kind_e2e_test.go`).

## Implementation notes (K5, 2026-09-05)

The NetworkPolicy enforcement probe, per-boot guest listener authentication, a
control plane inside the kind job, the quota/limit example, and the decision to
drop dm-crypt. Verified against a fresh kind cluster: the probe passed, all six
driver tests and both control-plane tests ran through the authenticated
listeners, and the leak check (now including Secrets and the canary) was clean.

### Departures from the plan

1. **The probe is an experiment, not an inspection.** The plan said "create a
   canary pod with a deny policy and confirm a connection is refused". A
   refused connection is also what a canary that never started, an image that
   never pulled, or a cluster-agent egress policy that does not permit the
   canary looks like. So the probe dials the canary under an ALLOW policy
   first, and only a dial that succeeded and then failed once the policy was
   rewritten to DENY counts as enforcement — two consecutive failures, because
   a policy controller programs the datapath asynchronously and one flaky
   connect must not pass for a verdict.
2. **The canary has to look like a machine to be reachable at all.** The
   cluster-agent's own NetworkPolicy permits egress only to the guest ports on
   pods labelled `managed-by=proteos`. The canary therefore carries that label
   and listens on 1024, but never the machine `component` label, so `List`
   and the capacity sum ignore it. Written down because the failure mode is
   the probe reporting a dead network on a healthy cluster.
3. **The guest secret is per BOOT, keyed by pod UID.** The plan said "passes at
   pod creation"; the question it did not answer is what an idempotent ensure
   against a running pod does. Rotating then would leave the guest holding a
   token the driver no longer presents, so the Secret is written only on the
   pod-create path, and the driver's token cache is keyed by the pod's UID so
   a recreated pod can never be answered from the previous boot's entry.
4. **Every listener is gated, loopback included — so the readiness probe
   presents the token.** Exempting 127.0.0.1 would have kept the curl probe
   but left the guest's own `/control` endpoint open to any process in the
   pod. The probe now speaks over bash's `/dev/tcp` and reads the token from
   the container's env, which an exec probe inherits even though the
   guest-agent removes the variable from ITS environment on startup.
5. **The token travels by Secret reference, never inline**, so `kubectl get pod
   -o yaml` does not print it, and the Role gets `secrets` get/create/update/
   delete but deliberately no `list`.
6. **dm-crypt dropped**, PodSecurity stays `baseline` — both for the reason in
   the stage description: neither is compatible with a root-in-sandbox guest
   on gVisor, which is the recommended configuration.
7. **The kind overlay narrows the agent's preview allowlist** (3000–8080) so
   the control-plane e2e can tell the agent's allowlist apart from the control
   plane's: 8080 is served, 9000 is minted by the control plane and refused by
   the agent. Production leaves both at the defaults.

### What the kind e2e found this time

0. **A machine mid-ensure looked stopped, and the control plane called that a
   failed boot.** Only the new control-plane suite could find it: the driver
   suite polls a machine after its ensure returns, while the poller polls the
   machine it is provisioning concurrently with the ensure it issued. An
   ensure creates the PVC, the policy, the Secret and then the pod, and a
   Status that lands in between sees a volume and no pod — which is exactly
   what a stopped machine is. The first control-plane test passed on the
   poller's tick timing; the second did not. The driver now tracks in-flight
   ensures in memory and reports `creating` for that window (also the window
   where a terminal pod is being replaced). Process-local, so it does not
   violate "the API server is the record": if the agent restarts mid-ensure
   the control plane re-ensures.
1. **`net.Pipe` cannot stand in for a guest in a `DialGuest` test.** The
   driver writes its preamble before returning the connection, and an
   unbuffered pipe blocks that write until the test reads — which it cannot do
   until `DialGuest` returns. The dialer test helper is a real loopback
   listener for that reason.
2. kind has enforced NetworkPolicy since v0.24 (kube-network-policies), which
   the earlier e2e comments had assumed without saying so. It means the probe
   is asserted in CI for free, and that every earlier kind run had in fact
   been exercising enforced policies.
