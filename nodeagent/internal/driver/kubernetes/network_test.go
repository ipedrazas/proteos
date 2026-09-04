package kubernetes_test

import (
	"context"
	"slices"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	api "github.com/tavon-ai/proteos/nodeagent/api"
	"github.com/tavon-ai/proteos/nodeagent/internal/driver"
	k8s "github.com/tavon-ai/proteos/nodeagent/internal/driver/kubernetes"
	"github.com/tavon-ai/proteos/nodeagent/internal/driver/kubernetes/fakecluster"
)

// policyFor ensures a machine with the given policy and returns the
// NetworkPolicy the driver wrote for it.
func policyFor(t *testing.T, id string, pol driver.NetworkPolicy) *networkingv1.NetworkPolicy {
	t.Helper()
	d, cluster := newDriver(t, 0)
	s := spec(id)
	s.NetworkPolicy = pol
	if _, err := d.EnsureRunning(context.Background(), s); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	np, err := cluster.NetworkingV1().NetworkPolicies(testNamespace).
		Get(context.Background(), "m-"+id, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get networkpolicy: %v", err)
	}
	return np
}

// On Firecracker the guest is unreachable because vsock has no network
// identity. Here it is a pod with unauthenticated TCP listeners, and this
// ingress rule is the only thing between another pod and someone's terminal.
func TestIngressIsRestrictedToTheClusterAgent(t *testing.T) {
	np := policyFor(t, "f0000000-0000-4000-8000-000000000001", driver.NetworkPolicy{})

	if !slices.Contains(np.Spec.PolicyTypes, networkingv1.PolicyTypeIngress) {
		t.Fatal("policy does not declare an Ingress policy type, so ingress is unrestricted")
	}
	if len(np.Spec.Ingress) != 1 {
		t.Fatalf("want exactly one ingress rule, got %d", len(np.Spec.Ingress))
	}
	rule := np.Spec.Ingress[0]
	if len(rule.From) != 1 {
		t.Fatalf("want exactly one permitted peer, got %d", len(rule.From))
	}
	peer := rule.From[0]
	if peer.PodSelector == nil ||
		peer.PodSelector.MatchLabels["app.kubernetes.io/name"] != "proteos-cluster-agent" {
		t.Errorf("ingress peer is not the cluster-agent pod: %+v", peer.PodSelector)
	}
	if peer.NamespaceSelector == nil ||
		peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "proteos-system" {
		t.Errorf("ingress peer is not scoped to the agent namespace: %+v", peer.NamespaceSelector)
	}
	if peer.IPBlock != nil {
		t.Error("ingress peer carries an ipBlock, which would widen it beyond the agent pod")
	}

	// Exactly the four guest ports, nothing else.
	var ports []int32
	for _, p := range rule.Ports {
		ports = append(ports, p.Port.IntVal)
	}
	slices.Sort(ports)
	want := []int32{
		int32(api.GuestTerminalPort), int32(api.GuestWebPort),
		int32(api.GuestPreviewPort), int32(api.GuestSSHPort),
	}
	slices.Sort(want)
	if !slices.Equal(ports, want) {
		t.Errorf("ingress ports = %v, want %v", ports, want)
	}
}

// Without the agent's pod labels the driver cannot name the one legitimate
// peer. It must then deny all ingress rather than write a selector that matches
// every pod in the namespace: an unreachable machine is a visible failure, a
// silently over-permissive policy is not.
func TestIngressFailsClosedWithoutAgentLabels(t *testing.T) {
	cluster := fakecluster.New(fakecluster.Options{Namespace: testNamespace})
	d := k8s.New(cluster, k8s.Config{Namespace: testNamespace}) // no AgentPodLabels
	const id = "f0000000-0000-4000-8000-000000000002"

	if _, err := d.EnsureRunning(context.Background(), spec(id)); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	np, err := cluster.NetworkingV1().NetworkPolicies(testNamespace).
		Get(context.Background(), "m-"+id, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get networkpolicy: %v", err)
	}
	if !slices.Contains(np.Spec.PolicyTypes, networkingv1.PolicyTypeIngress) {
		t.Fatal("ingress policy type missing: ingress would be unrestricted")
	}
	if len(np.Spec.Ingress) != 0 {
		t.Errorf("want no ingress rules (deny all), got %+v", np.Spec.Ingress)
	}
}

// Every mode keeps DNS working; a machine that cannot resolve names is
// indistinguishable from a broken one.
func TestEveryModeAllowsClusterDNS(t *testing.T) {
	for i, mode := range []string{
		api.NetworkPolicyAllowAll, api.NetworkPolicyDenyAll,
		api.NetworkPolicyAllowDomains, api.NetworkPolicyDenyDomains,
	} {
		t.Run(mode, func(t *testing.T) {
			id := "f1000000-0000-4000-8000-00000000000" + string(rune('1'+i))
			np := policyFor(t, id, driver.NetworkPolicy{Mode: mode, Domains: []string{"example.com"}})

			var found bool
			for _, rule := range np.Spec.Egress {
				for _, p := range rule.Ports {
					if p.Port != nil && p.Port.IntVal == 53 {
						found = true
					}
				}
			}
			if !found {
				t.Errorf("mode %q has no DNS egress rule", mode)
			}
		})
	}
}

// allow_all is "the internet, minus everything private" — the same shape the
// firecracker driver enforces with an nft forward drop. A machine must never be
// able to reach the node, its neighbours, or the cloud metadata endpoint that
// would hand it the node's credentials.
func TestAllowAllExcludesPrivateAndClusterRanges(t *testing.T) {
	np := policyFor(t, "f2000000-0000-4000-8000-000000000001",
		driver.NetworkPolicy{Mode: api.NetworkPolicyAllowAll})

	block := internetBlock(t, np)
	for _, cidr := range []string{
		"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
		"169.254.0.0/16", // cloud metadata
		"127.0.0.0/8",
		"10.244.0.0/16", // the configured cluster CIDR
	} {
		if !slices.Contains(block.Except, cidr) {
			t.Errorf("allow_all does not exclude %s (except = %v)", cidr, block.Except)
		}
	}
}

// deny_all is DNS and nothing else: no 0.0.0.0/0 rule at all.
func TestDenyAllHasNoInternetRule(t *testing.T) {
	np := policyFor(t, "f3000000-0000-4000-8000-000000000001",
		driver.NetworkPolicy{Mode: api.NetworkPolicyDenyAll})

	for _, rule := range np.Spec.Egress {
		for _, peer := range rule.To {
			if peer.IPBlock != nil && peer.IPBlock.CIDR == "0.0.0.0/0" {
				t.Fatal("deny_all wrote an allow-the-internet rule")
			}
		}
	}
	if len(np.Spec.Egress) != 1 {
		t.Errorf("deny_all should have exactly the DNS rule, got %d rules", len(np.Spec.Egress))
	}
}

// An allow_domains list that resolves to nothing (unreachable DNS in CI, a
// typo'd domain) must not silently degrade to allow_all.
func TestAllowDomainsNeverWidensToTheInternet(t *testing.T) {
	np := policyFor(t, "f4000000-0000-4000-8000-000000000001", driver.NetworkPolicy{
		Mode:    api.NetworkPolicyAllowDomains,
		Domains: []string{"this-domain-does-not-resolve.invalid"},
	})
	for _, rule := range np.Spec.Egress {
		for _, peer := range rule.To {
			if peer.IPBlock != nil && peer.IPBlock.CIDR == "0.0.0.0/0" {
				t.Fatal("allow_domains fell back to allowing the whole internet")
			}
		}
	}
}

// The policy is rewritten on every ensure, which is how a policy change reaches
// an already-created machine.
func TestPolicyIsUpdatedOnReEnsure(t *testing.T) {
	d, cluster := newDriver(t, 0)
	ctx := context.Background()
	const id = "f5000000-0000-4000-8000-000000000001"

	open := spec(id)
	open.NetworkPolicy = driver.NetworkPolicy{Mode: api.NetworkPolicyAllowAll}
	if _, err := d.EnsureRunning(ctx, open); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}

	locked := spec(id)
	locked.NetworkPolicy = driver.NetworkPolicy{Mode: api.NetworkPolicyDenyAll}
	if _, err := d.EnsureRunning(ctx, locked); err != nil {
		t.Fatalf("EnsureRunning (policy change): %v", err)
	}

	np, err := cluster.NetworkingV1().NetworkPolicies(testNamespace).
		Get(ctx, "m-"+id, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get networkpolicy: %v", err)
	}
	for _, rule := range np.Spec.Egress {
		for _, peer := range rule.To {
			if peer.IPBlock != nil && peer.IPBlock.CIDR == "0.0.0.0/0" {
				t.Fatal("the machine kept its old allow_all egress after being locked down")
			}
		}
	}
}

// internetBlock returns the single 0.0.0.0/0 egress block, failing if there
// isn't exactly one.
func internetBlock(t *testing.T, np *networkingv1.NetworkPolicy) *networkingv1.IPBlock {
	t.Helper()
	var found *networkingv1.IPBlock
	for _, rule := range np.Spec.Egress {
		for _, peer := range rule.To {
			if peer.IPBlock != nil && peer.IPBlock.CIDR == "0.0.0.0/0" {
				if found != nil {
					t.Fatal("more than one internet egress rule")
				}
				found = peer.IPBlock
			}
		}
	}
	if found == nil {
		t.Fatal("no internet egress rule")
	}
	return found
}
