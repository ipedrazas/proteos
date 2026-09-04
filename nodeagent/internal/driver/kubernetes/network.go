package kubernetes

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	api "github.com/tavon-ai/proteos/nodeagent/api"
	"github.com/tavon-ai/proteos/nodeagent/internal/driver"
)

// namespaceNameLabel is the well-known label the API server stamps on every
// namespace (NamespaceDefaultLabelName). It is how a NetworkPolicy names a
// namespace without the operator having to label anything by hand.
const namespaceNameLabel = "kubernetes.io/metadata.name"

// privateCIDRs are the ranges a machine may never reach: RFC1918, link-local
// (which is where cloud metadata services live — 169.254.169.254), loopback,
// and carrier-grade NAT. This mirrors the firecracker driver's nft forward
// drop, which is what stops a guest from reaching the host, its neighbours, or
// the metadata endpoint that would hand it the node's cloud credentials.
var privateCIDRs = []string{
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"169.254.0.0/16",
	"127.0.0.0/8",
	"100.64.0.0/10",
}

// domainResolveTimeout bounds the one-shot DNS resolution of a policy's domain
// list. Like the firecracker driver's nft ip sets, the result is a SNAPSHOT
// taken at boot: Kubernetes NetworkPolicy is IP/label-based, so an FQDN policy
// cannot follow a domain whose address changes later. K6 replaces this with a
// CNI DNS policy (Cilium/Calico).
const domainResolveTimeout = 5 * time.Second

// ensureNetworkPolicy writes the machine's NetworkPolicy. Unlike the pod, the
// policy IS updated on every ensure: it is cheap, it has no identity a running
// machine depends on, and it is how a policy change reaches an already-created
// machine.
//
// This is not optional hardening. On Firecracker the guest is unreachable
// because vsock has no network identity; here it is a pod with TCP listeners
// that answer anyone who can route to them — and those listeners are
// deliberately unauthenticated (the terminal, code-server, the preview forward).
// The ingress rule below is the whole reason that is still safe, which is why
// the cluster-agent refuses to start on a CNI that does not enforce policy
// (see Preflight).
func (d *Driver) ensureNetworkPolicy(ctx context.Context, spec driver.VMSpec) error {
	name := netpolName(spec.MachineID)
	policies := d.client.NetworkingV1().NetworkPolicies(d.cfg.Namespace)

	desired := d.buildNetworkPolicy(ctx, spec)

	existing, err := policies.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if _, err := policies.Create(ctx, desired, metav1.CreateOptions{}); err != nil &&
			!apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create networkpolicy %s: %w", name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("get networkpolicy %s: %w", name, err)
	}

	desired.ResourceVersion = existing.ResourceVersion
	if _, err := policies.Update(ctx, desired, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update networkpolicy %s: %w", name, err)
	}
	return nil
}

// buildNetworkPolicy renders one machine's ingress + egress rules.
func (d *Driver) buildNetworkPolicy(ctx context.Context, spec driver.VMSpec) *networkingv1.NetworkPolicy {
	meta := d.objectMeta(netpolName(spec.MachineID), spec)
	return &networkingv1.NetworkPolicy{
		ObjectMeta: meta,
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{MachineLabel: resourceName(spec.MachineID)},
			},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Ingress: d.ingressRules(),
			Egress:  d.egressRules(ctx, spec.NetworkPolicy),
		},
	}
}

// ingressRules permit exactly one peer — the cluster-agent pod — on the four
// guest ports, and nothing else. Every other pod in the cluster, including
// another machine, is denied.
//
// With no AgentPodLabels configured the driver cannot name that peer, and a
// rule with an empty pod selector would match EVERY pod in the agent's
// namespace. Rather than write a policy that quietly permits more than it
// looks like it does, the ingress list is then empty: with PolicyTypeIngress
// set that denies all ingress, and the machine simply becomes unreachable —
// a loud, safe failure. EnsureRunning logs the misconfiguration.
func (d *Driver) ingressRules() []networkingv1.NetworkPolicyIngressRule {
	if len(d.cfg.AgentPodLabels) == 0 {
		return nil
	}
	tcp := corev1TCP()
	ports := make([]networkingv1.NetworkPolicyPort, 0, 4)
	for _, p := range []uint32{
		api.GuestTerminalPort, api.GuestWebPort, api.GuestPreviewPort, api.GuestSSHPort,
	} {
		port := intstr.FromInt32(int32(p))
		ports = append(ports, networkingv1.NetworkPolicyPort{Protocol: &tcp, Port: &port})
	}
	return []networkingv1.NetworkPolicyIngressRule{{
		From: []networkingv1.NetworkPolicyPeer{{
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{namespaceNameLabel: d.cfg.AgentNamespace},
			},
			PodSelector: &metav1.LabelSelector{MatchLabels: d.cfg.AgentPodLabels},
		}},
		Ports: ports,
	}}
}

// egressRules map the four agentapi network-policy modes onto Kubernetes egress
// rules, matching what the firecracker driver enforces with nftables:
//
//	allow_all      internet, minus everything private
//	deny_all       DNS only (name resolution has to keep working, or the mode
//	               is indistinguishable from a broken machine)
//	allow_domains  only the snapshot of addresses those domains resolve to
//	deny_domains   internet, minus everything private, minus those addresses
//
// DNS to the cluster resolver is allowed in every mode, exactly as the nft
// rules leave the guest's resolver reachable.
func (d *Driver) egressRules(ctx context.Context, pol driver.NetworkPolicy) []networkingv1.NetworkPolicyEgressRule {
	rules := []networkingv1.NetworkPolicyEgressRule{d.dnsRule()}

	mode := pol.Mode
	if mode == "" {
		mode = api.NetworkPolicyAllowAll
	}

	switch mode {
	case api.NetworkPolicyDenyAll:
		// DNS only.

	case api.NetworkPolicyAllowDomains:
		ips := d.resolveDomains(ctx, pol.Domains)
		if len(ips) == 0 {
			slog.Warn("kubernetes: allow_domains policy resolved to no addresses — "+
				"the machine will have DNS but no egress", "domains", pol.Domains)
			break
		}
		peers := make([]networkingv1.NetworkPolicyPeer, 0, len(ips))
		for _, ip := range ips {
			peers = append(peers, networkingv1.NetworkPolicyPeer{
				IPBlock: &networkingv1.IPBlock{CIDR: ip + "/32"},
			})
		}
		rules = append(rules, networkingv1.NetworkPolicyEgressRule{To: peers})

	case api.NetworkPolicyDenyDomains:
		except := append([]string(nil), d.deniedCIDRs()...)
		for _, ip := range d.resolveDomains(ctx, pol.Domains) {
			except = append(except, ip+"/32")
		}
		rules = append(rules, networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{{
				IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: dedupe(except)},
			}},
		})

	default: // allow_all
		rules = append(rules, networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{{
				IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: d.deniedCIDRs()},
			}},
		})
	}
	return rules
}

// dnsRule allows UDP+TCP 53 to the cluster DNS namespace.
func (d *Driver) dnsRule() networkingv1.NetworkPolicyEgressRule {
	udp, tcp := corev1UDP(), corev1TCP()
	p53 := intstr.FromInt32(53)
	return networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{{
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{namespaceNameLabel: d.cfg.DNSNamespace},
			},
		}},
		Ports: []networkingv1.NetworkPolicyPort{
			{Protocol: &udp, Port: &p53},
			{Protocol: &tcp, Port: &p53},
		},
	}
}

// deniedCIDRs is the always-excluded set: RFC1918 and friends plus the
// operator-declared cluster CIDRs (pod and service networks), so a machine can
// reach the internet but never the cluster it runs in.
func (d *Driver) deniedCIDRs() []string {
	return dedupe(append(append([]string(nil), privateCIDRs...), d.cfg.ClusterCIDRs...))
}

// resolveDomains snapshots a domain list to IPv4 addresses. Failures are logged
// and skipped rather than failing the ensure: one unresolvable domain in an
// allow list should narrow the policy, not stop the machine from booting.
func (d *Driver) resolveDomains(ctx context.Context, domains []string) []string {
	if len(domains) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, domainResolveTimeout)
	defer cancel()

	seen := make(map[string]struct{})
	var out []string
	for _, domain := range domains {
		addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", domain)
		if err != nil {
			slog.Warn("kubernetes: could not resolve policy domain", "domain", domain, "err", err)
			continue
		}
		for _, a := range addrs {
			ip := a.Unmap()
			if !ip.Is4() || !routable(ip) {
				continue
			}
			s := ip.String()
			if _, dup := seen[s]; dup {
				continue
			}
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// routable rejects addresses that a machine must never be pointed at even when
// a domain resolves to one (a DNS-rebinding style answer of 127.0.0.1 or a
// metadata address would otherwise be written straight into an allow rule).
func routable(ip netip.Addr) bool {
	return !ip.IsLoopback() && !ip.IsPrivate() && !ip.IsLinkLocalUnicast() &&
		!ip.IsLinkLocalMulticast() && !ip.IsUnspecified()
}

// corev1TCP / corev1UDP return addressable protocol values: a
// NetworkPolicyPort takes a *corev1.Protocol, and a constant is not
// addressable.
func corev1TCP() corev1.Protocol { return corev1.ProtocolTCP }
func corev1UDP() corev1.Protocol { return corev1.ProtocolUDP }

func dedupe(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
