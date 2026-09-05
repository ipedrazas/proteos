package kubernetes_test

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	k8s "github.com/tavon-ai/proteos/nodeagent/internal/driver/kubernetes"
	"github.com/tavon-ai/proteos/nodeagent/internal/driver/kubernetes/fakecluster"
)

// The NetworkPolicy enforcement probe (K5, deferred from K2). A fake cluster
// routes no packets, so the CNI under test is a dialer: one that consults the
// canary's policy behaves like an enforcing CNI, one that ignores it behaves
// like flannel. What is being tested is the experiment's logic — that it
// requires the permitted dial to succeed before it trusts the denied dial's
// failure, that it reaches the right verdict either way, and that it cleans up.

// cniDialer stands in for the cluster network. enforcing decides whether a
// dial to the canary honours the canary's current NetworkPolicy.
type cniDialer struct {
	cluster   *fakecluster.Cluster
	enforcing bool
	// down makes every dial fail, as a cluster where pod-to-pod traffic from
	// the agent is blocked outright would.
	down bool
}

func (c *cniDialer) dial(ctx context.Context, _, addr string) (net.Conn, error) {
	if c.down {
		return nil, errors.New("connect: connection timed out")
	}
	if !c.enforcing {
		return pipeEnd(), nil
	}
	// Enforcing: find the policy selecting the canary and honour it.
	pols, err := c.cluster.NetworkingV1().NetworkPolicies(testNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/component=netpol-canary",
	})
	if err != nil {
		return nil, err
	}
	for _, p := range pols.Items {
		if len(p.Spec.Ingress) == 0 {
			return nil, errors.New("connect: connection timed out (dropped by policy)")
		}
	}
	return pipeEnd(), nil
}

func pipeEnd() net.Conn {
	a, b := net.Pipe()
	go func() { _ = b.Close() }()
	return a
}

func canaryObjects(t *testing.T, cluster *fakecluster.Cluster) (pods, policies int) {
	t.Helper()
	ctx := context.Background()
	pl, err := cluster.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/component=netpol-canary",
	})
	if err != nil {
		t.Fatal(err)
	}
	nl, err := cluster.NetworkingV1().NetworkPolicies(testNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/component=netpol-canary",
	})
	if err != nil {
		t.Fatal(err)
	}
	return len(pl.Items), len(nl.Items)
}

func probeDriver(t *testing.T, cni *cniDialer) (*k8s.Driver, *fakecluster.Cluster) {
	t.Helper()
	d, cluster := newDriver(t, 10*time.Millisecond)
	cni.cluster = cluster
	// The fake's config sets the production defaults; shorten the wait for a
	// deny policy to bite so the "not enforced" verdict arrives in test time.
	d = k8s.New(cluster, k8s.Config{
		Namespace:                 testNamespace,
		AgentNamespace:            "proteos-system",
		AgentPodLabels:            map[string]string{"app.kubernetes.io/name": "proteos-cluster-agent"},
		NetpolProbeEnforceTimeout: 1500 * time.Millisecond,
	}).WithDialer(cni.dial)
	return d, cluster
}

func TestPreflightPassesOnAnEnforcingCNIAndCleansUp(t *testing.T) {
	cni := &cniDialer{enforcing: true}
	d, cluster := probeDriver(t, cni)

	if err := d.Preflight(context.Background()); err != nil {
		t.Fatalf("Preflight on an enforcing CNI: %v", err)
	}
	if pods, pols := canaryObjects(t, cluster); pods != 0 || pols != 0 {
		t.Fatalf("canary leaked: %d pods, %d policies left in the machine namespace", pods, pols)
	}
	// The machine list must never have seen it.
	list, err := d.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("the canary was counted as a machine: %+v", list)
	}
}

func TestPreflightRefusesToStartOnANonEnforcingCNI(t *testing.T) {
	cni := &cniDialer{enforcing: false}
	d, cluster := probeDriver(t, cni)

	err := d.Preflight(context.Background())
	if !errors.Is(err, k8s.ErrNetworkPolicyNotEnforced) {
		t.Fatalf("Preflight = %v, want ErrNetworkPolicyNotEnforced", err)
	}
	if !strings.Contains(err.Error(), "PROTEOS_K8S_SKIP_NETPOL_PROBE") {
		t.Errorf("the refusal does not tell the operator how to override it: %v", err)
	}
	if pods, pols := canaryObjects(t, cluster); pods != 0 || pols != 0 {
		t.Fatalf("canary leaked after a failed probe: %d pods, %d policies", pods, pols)
	}
}

// A canary that is unreachable even when permitted is NOT evidence of
// enforcement — it is a broken network or a broken image — and must be reported
// as that rather than as a pass.
func TestPreflightDoesNotMistakeADeadNetworkForEnforcement(t *testing.T) {
	cni := &cniDialer{down: true}
	d, cluster := probeDriver(t, cni)

	// Shorten the ready wait through the probe's error path: with every dial
	// failing it would otherwise wait the full canary timeout. Use a context
	// deadline as the bound.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := d.Preflight(ctx)
	if err == nil {
		t.Fatal("Preflight passed on a network where the canary was never reachable")
	}
	if errors.Is(err, k8s.ErrNetworkPolicyNotEnforced) {
		t.Fatalf("a dead network was reported as a non-enforcing CNI: %v", err)
	}
	if pods, pols := canaryObjects(t, cluster); pods != 0 || pols != 0 {
		t.Fatalf("canary leaked: %d pods, %d policies", pods, pols)
	}
}

func TestPreflightSkipsTheProbeWhenToldTo(t *testing.T) {
	cni := &cniDialer{enforcing: false} // would fail if probed
	_, cluster := newDriver(t, 0)
	cni.cluster = cluster
	d := k8s.New(cluster, k8s.Config{
		Namespace:       testNamespace,
		AgentPodLabels:  map[string]string{"app.kubernetes.io/name": "proteos-cluster-agent"},
		SkipNetpolProbe: true,
	}).WithDialer(cni.dial)
	if err := d.Preflight(context.Background()); err != nil {
		t.Fatalf("Preflight with the probe skipped: %v", err)
	}
	if pods, _ := canaryObjects(t, cluster); pods != 0 {
		t.Fatal("the probe ran despite SkipNetpolProbe")
	}
}

// The canary is written so that the cluster-agent's OWN NetworkPolicy lets the
// agent reach it: that policy permits egress only to guest ports on pods
// labelled managed-by=proteos. A canary without that label or on another port
// would be unreachable for a reason that has nothing to do with the CNI.
func TestCanaryIsReachableUnderTheAgentsOwnEgressPolicy(t *testing.T) {
	var seen []string
	d, cluster := newDriver(t, 0)
	d = k8s.New(cluster, k8s.Config{
		Namespace:                 testNamespace,
		AgentPodLabels:            map[string]string{"app.kubernetes.io/name": "proteos-cluster-agent"},
		NetpolProbeEnforceTimeout: 500 * time.Millisecond,
	}).WithDialer(func(ctx context.Context, _, addr string) (net.Conn, error) {
		seen = append(seen, addr)
		pods, err := cluster.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: "app.kubernetes.io/component=netpol-canary",
		})
		if err != nil {
			return nil, err
		}
		for _, p := range pods.Items {
			if p.Labels["app.kubernetes.io/managed-by"] != "proteos" {
				return nil, errors.New("the agent's egress policy would not permit this peer")
			}
			if p.Labels["app.kubernetes.io/component"] == "machine" {
				return nil, errors.New("the canary must not pose as a machine")
			}
		}
		// Behave as an enforcing CNI otherwise.
		pols, _ := cluster.NetworkingV1().NetworkPolicies(testNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: "app.kubernetes.io/component=netpol-canary",
		})
		for _, p := range pols.Items {
			if len(p.Spec.Ingress) == 0 {
				return nil, errors.New("dropped")
			}
		}
		return pipeEnd(), nil
	})
	if err := d.Preflight(context.Background()); err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	for _, addr := range seen {
		if !strings.HasSuffix(addr, ":1024") {
			t.Fatalf("canary dialed on %s; the agent's egress policy only permits the guest ports", addr)
		}
	}
}
