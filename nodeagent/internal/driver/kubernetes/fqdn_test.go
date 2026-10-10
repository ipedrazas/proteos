package kubernetes_test

import (
	"context"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	api "github.com/ipedrazas/proteos/nodeagent/api"
	"github.com/ipedrazas/proteos/nodeagent/internal/driver"
	k8s "github.com/ipedrazas/proteos/nodeagent/internal/driver/kubernetes"
	"github.com/ipedrazas/proteos/nodeagent/internal/driver/kubernetes/fakecluster"
)

// The FQDN engine (K6). A fake cluster cannot resolve a name through a DNS
// proxy, so what is tested here is the object the driver writes and when: the
// CiliumNetworkPolicy exists exactly for allow_domains machines, names the
// resolver with an L7 DNS rule (without which Cilium never sees the answers),
// maps a plain domain to matchName and a wildcard to matchPattern, and leaves
// with the machine. Whether Cilium then follows the names is a question for a
// real Cilium cluster (e2e_kind_test.go, TestKindFQDNEgress).

var cnpGVR = schema.GroupVersionResource{Group: "cilium.io", Version: "v2", Resource: "ciliumnetworkpolicies"}

// fqdnDriver is newDriver plus a fake dynamic client and a discovery that
// serves the Cilium CRD, with the engine on unless engine is "".
func fqdnDriver(t *testing.T, engine string) (*k8s.Driver, *fakecluster.Cluster, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	cluster := fakecluster.New(fakecluster.Options{Namespace: testNamespace})
	disc := cluster.Discovery().(*fakediscovery.FakeDiscovery)
	disc.Resources = append(disc.Resources, &metav1.APIResourceList{
		GroupVersion: "cilium.io/v2",
		APIResources: []metav1.APIResource{{Name: "ciliumnetworkpolicies", Namespaced: true, Kind: "CiliumNetworkPolicy"}},
	})
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{cnpGVR: "CiliumNetworkPolicyList"})
	d := k8s.New(cluster, k8s.Config{
		Namespace:      testNamespace,
		AgentNamespace: "proteos-system",
		AgentPodLabels: map[string]string{"app.kubernetes.io/name": "proteos-cluster-agent"},
		DNSNamespace:   "kube-system",
		FQDNPolicy:     engine,
	}).WithDynamicClient(dyn)
	return d, cluster, dyn
}

func getCNP(t *testing.T, dyn *dynamicfake.FakeDynamicClient, id string) (*unstructured.Unstructured, error) {
	t.Helper()
	return dyn.Resource(cnpGVR).Namespace(testNamespace).Get(context.Background(), "m-"+id, metav1.GetOptions{})
}

func TestAllowDomainsWritesACiliumPolicyThatFollowsNames(t *testing.T) {
	d, _, dyn := fqdnDriver(t, k8s.FQDNPolicyCilium)
	ctx := context.Background()
	const id = "fd000000-0000-4000-8000-000000000001"

	s := spec(id)
	s.NetworkPolicy = driver.NetworkPolicy{Mode: api.NetworkPolicyAllowDomains, Domains: []string{"github.com", "*.Githubusercontent.com"}}
	if _, err := d.EnsureRunning(ctx, s); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}

	cnp, err := getCNP(t, dyn, id)
	if err != nil {
		t.Fatalf("no CiliumNetworkPolicy for an allow_domains machine: %v", err)
	}
	if cnp.GetLabels()["app.kubernetes.io/managed-by"] != "proteos" {
		t.Error("the CNP does not carry the managed-by label, so nothing would ever clean it up by selector")
	}
	sel, _, _ := unstructured.NestedStringMap(cnp.Object, "spec", "endpointSelector", "matchLabels")
	if sel["proteos.io/machine"] != "m-"+id {
		t.Errorf("endpointSelector = %v, want this machine only", sel)
	}

	egress, _, _ := unstructured.NestedSlice(cnp.Object, "spec", "egress")
	if len(egress) != 2 {
		t.Fatalf("want 2 egress rules (DNS proxy, FQDNs), got %d: %v", len(egress), egress)
	}

	// Rule 0: the resolver, with an L7 dns section — the switch that turns the
	// proxy on for this pod.
	dns := egress[0].(map[string]any)
	toEndpoints := dns["toEndpoints"].([]any)[0].(map[string]any)["matchLabels"].(map[string]any)
	if toEndpoints["k8s:io.kubernetes.pod.namespace"] != "kube-system" || toEndpoints["k8s:k8s-app"] != "kube-dns" {
		t.Errorf("DNS rule selects %v, want kube-dns in kube-system", toEndpoints)
	}
	toPorts := dns["toPorts"].([]any)[0].(map[string]any)
	if rules, ok := toPorts["rules"].(map[string]any); !ok || rules["dns"] == nil {
		t.Error("DNS rule has no L7 dns section: Cilium would never observe the machine's lookups and toFQDNs would match nothing")
	}

	// Rule 1: the names. Lower-cased, plain → matchName, wildcard → matchPattern.
	fqdns := egress[1].(map[string]any)["toFQDNs"].([]any)
	var got []string
	for _, f := range fqdns {
		for k, v := range f.(map[string]any) {
			got = append(got, k+"="+v.(string))
		}
	}
	want := "matchName=github.com matchPattern=*.githubusercontent.com"
	if strings.Join(got, " ") != want {
		t.Errorf("toFQDNs = %q, want %q", strings.Join(got, " "), want)
	}
	if _, has := egress[1].(map[string]any)["toPorts"]; has {
		t.Error("the FQDN rule restricts ports; the snapshot ipBlocks it complements do not")
	}
}

// Only allow_domains has anything to follow. Every other mode is fully said by
// the NetworkPolicy, and a CNP left over from an earlier allow_domains would
// widen it — so a mode change must remove it.
func TestOtherModesHaveNoCiliumPolicyAndAModeChangeRemovesIt(t *testing.T) {
	d, _, dyn := fqdnDriver(t, k8s.FQDNPolicyCilium)
	ctx := context.Background()

	for i, mode := range []string{api.NetworkPolicyAllowAll, api.NetworkPolicyDenyAll, api.NetworkPolicyDenyDomains} {
		id := "fd000000-0000-4000-8000-00000000001" + string(rune('0'+i))
		s := spec(id)
		s.NetworkPolicy = driver.NetworkPolicy{Mode: mode, Domains: []string{"example.com"}}
		if _, err := d.EnsureRunning(ctx, s); err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		if _, err := getCNP(t, dyn, id); !apierrors.IsNotFound(err) {
			t.Errorf("%s: a CiliumNetworkPolicy was written (err=%v); only allow_domains has names to follow", mode, err)
		}
	}

	const id = "fd000000-0000-4000-8000-000000000020"
	s := spec(id)
	s.NetworkPolicy = driver.NetworkPolicy{Mode: api.NetworkPolicyAllowDomains, Domains: []string{"example.com"}}
	if _, err := d.EnsureRunning(ctx, s); err != nil {
		t.Fatal(err)
	}
	if _, err := getCNP(t, dyn, id); err != nil {
		t.Fatalf("allow_domains: %v", err)
	}
	s.NetworkPolicy = driver.NetworkPolicy{Mode: api.NetworkPolicyAllowAll}
	if _, err := d.EnsureRunning(ctx, s); err != nil {
		t.Fatal(err)
	}
	if _, err := getCNP(t, dyn, id); !apierrors.IsNotFound(err) {
		t.Errorf("after the mode changed to allow_all the CNP is still there (err=%v)", err)
	}

	// Destroy takes it too.
	s.NetworkPolicy = driver.NetworkPolicy{Mode: api.NetworkPolicyAllowDomains, Domains: []string{"example.com"}}
	if _, err := d.EnsureRunning(ctx, s); err != nil {
		t.Fatal(err)
	}
	if err := d.Destroy(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := getCNP(t, dyn, id); !apierrors.IsNotFound(err) {
		t.Errorf("the CNP survived Destroy (err=%v)", err)
	}
}

// With no engine configured the driver must not touch the CRD at all — the
// Role may not even grant it, and the snapshot NetworkPolicy is the whole
// policy, exactly as before K6.
func TestNoEngineMeansNoCiliumObjects(t *testing.T) {
	d, _, dyn := fqdnDriver(t, "")
	const id = "fd000000-0000-4000-8000-000000000030"
	s := spec(id)
	s.NetworkPolicy = driver.NetworkPolicy{Mode: api.NetworkPolicyAllowDomains, Domains: []string{"example.com"}}
	if _, err := d.EnsureRunning(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if len(dyn.Actions()) != 0 {
		t.Errorf("the dynamic client was used with no engine configured: %v", dyn.Actions())
	}
	if _, err := getCNP(t, dyn, id); !apierrors.IsNotFound(err) {
		t.Errorf("a CNP was written with no engine configured (err=%v)", err)
	}
}

// Asking for an engine the cluster does not have is a startup error, not a
// per-machine ensure failure hours later.
func TestPreflightRefusesAnEngineTheClusterDoesNotServe(t *testing.T) {
	cluster := fakecluster.New(fakecluster.Options{Namespace: testNamespace})
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	d := k8s.New(cluster, k8s.Config{
		Namespace:       testNamespace,
		AgentPodLabels:  map[string]string{"app.kubernetes.io/name": "proteos-cluster-agent"},
		FQDNPolicy:      k8s.FQDNPolicyCilium,
		SkipNetpolProbe: true,
	}).WithDynamicClient(dyn)

	err := d.Preflight(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cilium.io/v2") {
		t.Fatalf("Preflight = %v, want a refusal naming the missing cilium.io/v2 API", err)
	}

	// And with the CRD served, it starts.
	disc := cluster.Discovery().(*fakediscovery.FakeDiscovery)
	disc.Resources = append(disc.Resources, &metav1.APIResourceList{
		GroupVersion: "cilium.io/v2",
		APIResources: []metav1.APIResource{{Name: "ciliumnetworkpolicies", Namespaced: true, Kind: "CiliumNetworkPolicy"}},
	})
	if err := d.Preflight(context.Background()); err != nil {
		t.Fatalf("Preflight with the CRD served: %v", err)
	}
}
