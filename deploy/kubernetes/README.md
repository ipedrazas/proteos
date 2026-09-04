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
| `overlays/kind/` | The e2e shape: no RuntimeClass, locally-loaded images. Used by `e2e-kind.sh`, not for production |

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

It is a **functional** test, not an isolation test — kind has no gVisor, so the
overlay disables the RuntimeClass. What it does prove is the part unit tests
cannot: that pods schedule, PVCs bind and survive a stop, the guest tunnel
reaches the pod IP, and the RBAC in `base/rbac.yaml` is sufficient.

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
to the cluster-agent pod, and that restriction is the only thing between one
tenant's pod and another tenant's terminal. On a CNI that ignores
NetworkPolicy (plain flannel, some managed defaults) the objects are accepted
and silently do nothing. Calico, Cilium, Antrea and GKE Dataplane V2 all
enforce it. Verify before you trust it:

```sh
kubectl -n proteos-machines run probe --image=curlimages/curl --restart=Never -- \
  sh -c 'curl -m 3 -s http://<machine-pod-ip>:1024/healthz; echo "exit=$?"'
# Expected: a timeout. A 200 means your CNI is not enforcing policy.
```

**4. An encrypted StorageClass.** The kubernetes backend cannot use the
per-machine LUKS volume key (there is no host to `luksOpen` on), so encryption
at rest is the StorageClass's job. Set `PROTEOS_K8S_STORAGE_CLASS` to an
encrypted one.

**5. `PROTEOS_K8S_CLUSTER_CIDRS`.** Machine egress always denies RFC1918,
link-local (cloud metadata) and loopback, but the pod and service CIDRs are
cluster-specific. Set them, or a machine can reach cluster services on a
non-RFC1918 range.

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

## Known differences from a Firecracker host

| | Firecracker | Kubernetes |
| --- | --- | --- |
| Isolation | microVM (own kernel) | sandboxed runtime, or the node's kernel |
| Hibernate | RAM snapshot, resume | poweroff; the volume is kept, the machine boots cold. Stop moves the machine through `stopping`, not `hibernating`, and the console hides the snapshot field |
| Encryption at rest | per-machine LUKS key | StorageClass |
| Egress policy | nftables on the tap | NetworkPolicy, IP snapshot (no live FQDN) |
| Inbound SSH | sshd under systemd | not yet — K4 adds an sshd sidecar |
