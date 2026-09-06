# Security Policy

ProteOS runs untrusted, autonomous AI coding agents inside hardware-isolated
Firecracker microVMs. The isolation boundary is the core security property of the
project, so we take vulnerability reports seriously — especially anything that
could let an agent escape its microVM, cross between users/machines, or reach the
control plane, gateway, or other tenants.

## Reporting a vulnerability

**Please do not report security issues through public GitHub issues, pull
requests, or discussions.**

Instead, report privately through one of:

- **GitHub Security Advisories** (preferred) — use the
  [**Report a vulnerability**](https://github.com/tavon-ai/proteos/security/advisories/new)
  button on the Security tab. This opens a private channel with the maintainers.
- **Email** — `security@tavon.ai` _(maintainers: replace with a monitored
  address before publishing if this mailbox does not exist)_.

Please include, as far as you can:

- a description of the issue and its impact;
- the affected component (control plane, node-agent, guest-agent, gateway, CLI,
  web) and version/commit;
- step-by-step reproduction, proof-of-concept, or affected code paths;
- any suggested remediation.

## What to expect

- We aim to **acknowledge** a report within **3 business days**.
- We'll work with you to confirm the issue and determine its severity and scope.
- We'll keep you updated on remediation progress and coordinate a disclosure
  timeline. We ask that you give us a reasonable window to ship a fix before any
  public disclosure.
- With your consent, we're happy to credit you in the advisory and release notes.

## Isolation model per backend

ProteOS's core security claim is that an AI coding agent runs behind a hardware
VM boundary rather than a shared kernel. A machine can therefore run its agent
with `--dangerously-skip-permissions`, an unauthenticated in-guest terminal, an
unauthenticated code-server, and passwordless sudo, because none of those are
reachable: the guest has no inbound network surface at all, only vsock, and
vsock reaches exactly one process on the host.

The Kubernetes backend (a machine is a Pod, see
[`deploy/kubernetes/`](deploy/kubernetes/README.md)) changes several of those
premises at once. It is a supported backend, not a lesser one, but an operator
must know precisely what differs.

| Property | Firecracker | Kubernetes |
| --- | --- | --- |
| Kernel | one per machine | shared with the node, unless a sandboxed RuntimeClass (gVisor/Kata) is installed |
| Guest reachability | vsock only; no listening socket on any network | TCP listeners on the pod IP, restricted by NetworkPolicy |
| Guest listener auth | none needed (vsock is the boundary) | a per-boot secret the cluster-agent presents first on every connection; the guest closes anything else |
| NetworkPolicy enforcement | n/a | proved at startup by a canary the agent must fail to reach once denied; the agent refuses to start otherwise |
| Inbound SSH | sshd under systemd, bound to 127.0.0.1 | sshd in a sidecar container, bound to the pod's 127.0.0.1 |
| Encryption at rest | per-machine LUKS key, held by the control plane and never written to the host | the StorageClass's responsibility |
| Egress control | nftables on the machine's tap | NetworkPolicy, from an IP snapshot taken at boot; on Cilium, `allow_domains` follows names live through the DNS proxy |
| Passwordless sudo in the guest | yes | not installed in the image |

What this means in practice:

- **A sandboxed RuntimeClass is a precondition, not hardening.** The
  cluster-agent defaults to `gvisor` and refuses to start if that RuntimeClass
  is absent. Setting `PROTEOS_K8S_RUNTIME_CLASS` to the empty string starts it
  anyway, logs loudly, and gives every machine the node's kernel. Do that only
  with the trade-off understood.
- **NetworkPolicy enforcement is load-bearing, and the agent proves it.** Each
  machine's policy permits ingress on the guest ports from the cluster-agent
  pod and nothing else. On a CNI that accepts NetworkPolicy objects without
  enforcing them (plain flannel, some managed defaults) those objects silently
  do nothing and any pod in the cluster can reach any machine's terminal. Since
  nothing in the data path can observe that, the cluster-agent runs an
  experiment at startup: a canary pod it can reach while a policy permits it,
  and must fail to reach once the policy denies it. If the deny changes
  nothing the agent refuses to start. `PROTEOS_K8S_SKIP_NETPOL_PROBE=1` skips
  it, loudly.
- **The guest's listeners are authenticated.** Every machine is started with a
  random per-boot secret, delivered by Kubernetes Secret, that the
  cluster-agent writes as the first line of every tunnel connection; the guest
  closes a connection that opens with anything else before a byte reaches the
  terminal, code-server, the preview forwarder or the ssh forward. This is the
  second control behind NetworkPolicy, independent of the CNI: a pod that can
  route to a machine still cannot speak to it. The secret is readable by
  anything with `secrets/get` in the machine namespace — the cluster-agent's
  Role, and cluster admins — which is a much smaller set than "anything that
  can route to a pod IP". `PROTEOS_K8S_GUEST_AUTH=0` disables it for a guest
  image that predates the preamble.
- **The per-machine volume key is not used.** There is no host on which to
  `luksOpen` it, so the driver accepts it, ignores it, and logs the fact. Use an
  encrypted StorageClass to restore encryption at rest.
- **SSH does not widen the surface.** sshd runs as a second container in the
  machine pod and binds the pod's loopback, which nothing outside that pod can
  address — the same property the microVM's `ListenAddress 127.0.0.1` gives. The
  only route to it is the guest agent's forward on the machine's SSH port, which
  the cluster-agent reaches under the same NetworkPolicy as every other guest
  port, and which the control-plane gateway authenticates. Authentication is
  key-only against the keys the control plane injects; the image contains no
  passwords, and sshd is configured with every kind of forwarding disabled so it
  cannot become a second egress path around the machine's NetworkPolicy. Host
  keys are generated per machine on its own volume — never baked into the image,
  where every machine would share one identity.
- **Egress policy is a snapshot, unless Cilium.** `allow_domains` /
  `deny_domains` resolve to addresses once, when the machine boots; a domain
  that later moves is not followed. On a Cilium cluster with
  `PROTEOS_K8S_FQDN_POLICY=cilium` the agent adds a `CiliumNetworkPolicy` whose
  `toFQDNs` rules follow the names an `allow_domains` machine resolves, through
  Cilium's DNS proxy — an ALLOW that is at least as narrow as the snapshot,
  never wider, since it admits only addresses the machine's own lookups
  returned for a listed name. `deny_domains` remains a snapshot everywhere:
  Cilium's deny rules take no FQDNs, and open-source Calico has no DNS policy.
  A machine that resolves a name by other means (a hard-coded address, DNS over
  HTTPS) is not followed, by design — and cannot reach it either, because the
  snapshot did not include it.

Machine egress always denies RFC1918, loopback and link-local — including
`169.254.169.254`, so a machine cannot read the node's cloud-metadata
credentials — plus the operator-declared cluster CIDRs.

## Scope

Areas of particular interest:

- microVM isolation / guest-to-host escape (and, on the Kubernetes backend,
  sandbox escape or cross-machine reachability)
- cross-tenant access (one user/machine reaching another's data or VM)
- authentication and authorization on the control plane and gateway
- secret handling (OpenBao integration, provider API keys, GitHub tokens)
- the port-preview and editor gateway paths

Reports about dependencies should ideally include how the vulnerable code path is
reachable in ProteOS.

Thank you for helping keep ProteOS and its users safe.
