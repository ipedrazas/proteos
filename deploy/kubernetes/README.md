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
| `namespace.yaml` | `proteos-system` (the agent) and `proteos-machines` (the machines) |
| `rbac.yaml` | ServiceAccount + namespaced Role (pods/PVCs/NetworkPolicies) + ClusterRole (nodes, for capacity) |
| `deployment.yaml` | The cluster-agent itself |
| `service.yaml` | ClusterIP :9090 |
| `networkpolicy.yaml` | Default-deny for `proteos-machines`; the agent's own policy |
| `runtimeclass-gvisor.yaml` | Example RuntimeClass (**not** applied by default — see below) |
| `kustomization.yaml` | Ties them together |

```sh
kubectl apply -k deploy/kubernetes/
```

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

## Known differences from a Firecracker host

| | Firecracker | Kubernetes |
| --- | --- | --- |
| Isolation | microVM (own kernel) | sandboxed runtime, or the node's kernel |
| Hibernate | RAM snapshot, resume | poweroff; the volume is kept, the machine boots cold |
| Encryption at rest | per-machine LUKS key | StorageClass |
| Egress policy | nftables on the tap | NetworkPolicy, IP snapshot (no live FQDN) |
| Inbound SSH | sshd under systemd | not yet — K4 adds an sshd sidecar |
