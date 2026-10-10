package kubernetes

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	api "github.com/ipedrazas/proteos/nodeagent/api"
)

// NetworkPolicy enforcement probe (K-plan decision 7, deferred from K2 and
// landed in K5).
//
// A NetworkPolicy object is accepted by every API server and enforced by only
// some CNIs. On the rest — plain flannel, some managed defaults — the object
// exists, kubectl shows it, and it does nothing: every pod in the cluster can
// reach every machine's terminal. Nothing in the data path reports this, which
// is exactly why the driver has to find out for itself before it serves a
// single machine. The probe is a controlled experiment rather than an
// inspection: it starts a canary listener in the machine namespace, proves it
// can reach it while a policy PERMITS the connection, then rewrites the policy
// to DENY it and requires the same dial to fail. A policy that works and a
// connection that fails for an unrelated reason look identical, so the
// permitted dial comes first.
//
// The canary carries the machine labels so that the cluster-agent's own egress
// policy — which permits only the guest ports on managed-by=proteos pods — lets
// the agent reach it at all, and listens on the terminal port for the same
// reason. It never carries the machine COMPONENT label, so List and the
// capacity sum ignore it.

const (
	canaryComponent = "netpol-canary"
	canaryLabel     = "proteos.io/netpol-canary"
	canaryPort      = api.GuestTerminalPort

	// DefaultNetpolProbeImage is the listener the canary runs. agnhost is the
	// image Kubernetes' own e2e suite uses for precisely this; `netexec` is an
	// HTTP server that accepts on the given port. Overridable for clusters that
	// cannot reach registry.k8s.io.
	DefaultNetpolProbeImage = "registry.k8s.io/e2e-test-images/agnhost:2.53"

	// canaryReadyTimeout bounds scheduling + image pull + start of the canary.
	canaryReadyTimeout = 3 * time.Minute

	// defaultEnforceTimeout bounds how long a deny policy may take to become
	// effective. Policy controllers program the datapath asynchronously; a few
	// seconds is typical, tens of seconds is a slow cluster, and a dial that
	// still succeeds after this is a CNI that is not going to enforce it.
	defaultEnforceTimeout = 60 * time.Second

	// canaryDialTimeout is one connection attempt. Enforcing CNIs DROP rather
	// than reject, so "blocked" arrives as a timeout, and this is how long a
	// blocked attempt takes to report.
	canaryDialTimeout = 3 * time.Second
)

// ErrNetworkPolicyNotEnforced is the probe's verdict when the deny policy
// changed nothing. Preflight returns it wrapped, and the agent refuses to start.
var ErrNetworkPolicyNotEnforced = errors.New("the CNI does not enforce NetworkPolicy")

// probeNetworkPolicyEnforcement runs the experiment. It cleans up after itself
// whatever the outcome; a leaked canary would count as a machine object in the
// namespace and is treated as a bug by the e2e.
func (d *Driver) probeNetworkPolicyEnforcement(ctx context.Context) error {
	if len(d.cfg.AgentPodLabels) == 0 {
		// Without the agent's labels no ALLOW rule can name it, so the permitted
		// half of the experiment cannot be written. Machines are created with
		// ingress denied in this configuration (see ingressRules), which is
		// already the safe failure; Preflight has warned about it.
		slog.Warn("kubernetes: skipping the NetworkPolicy enforcement probe — PROTEOS_K8S_AGENT_POD_LABELS is unset, so no allow rule can be written")
		return nil
	}

	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return fmt.Errorf("netpol probe: %w", err)
	}
	name := "proteos-netpol-canary-" + hex.EncodeToString(suffix[:])
	defer d.cleanupCanary(name)

	pods := d.client.CoreV1().Pods(d.cfg.Namespace)
	policies := d.client.NetworkingV1().NetworkPolicies(d.cfg.Namespace)

	// 1. A policy that PERMITS the agent, then the canary.
	if _, err := policies.Create(ctx, d.canaryPolicy(name, true), metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("netpol probe: create allow policy: %w", err)
	}
	if _, err := pods.Create(ctx, d.canaryPod(name), metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("netpol probe: create canary pod: %w", err)
	}

	addr, err := d.waitCanaryReachable(ctx, name)
	if err != nil {
		return err
	}

	// 2. Rewrite it to DENY everything, and wait for that to bite.
	existing, err := policies.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("netpol probe: get policy: %w", err)
	}
	deny := d.canaryPolicy(name, false)
	deny.ResourceVersion = existing.ResourceVersion
	if _, err := policies.Update(ctx, deny, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("netpol probe: update policy to deny: %w", err)
	}

	enforceTimeout := d.cfg.NetpolProbeEnforceTimeout
	if enforceTimeout <= 0 {
		enforceTimeout = defaultEnforceTimeout
	}
	start := time.Now()
	deadline := start.Add(enforceTimeout)
	failures := 0
	for {
		if err := d.tryDial(ctx, addr); err != nil {
			// Two in a row, so one flaky connect on a slow node cannot pass
			// for enforcement.
			failures++
			if failures >= 2 {
				slog.Info("kubernetes: NetworkPolicy enforcement verified — the CNI blocked the canary once its policy denied ingress",
					"took", time.Since(start).Round(100*time.Millisecond).String(), "canary", name)
				return nil
			}
		} else {
			failures = 0
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: a canary pod in %s stayed reachable for %s after its NetworkPolicy denied all ingress. "+
				"Every machine's terminal would be open to any pod in the cluster. Install a policy-enforcing CNI "+
				"(Calico, Cilium, Antrea, kube-router, GKE Dataplane V2, kind ≥ 0.24), or set PROTEOS_K8S_SKIP_NETPOL_PROBE=1 "+
				"to run anyway with that understood — see SECURITY.md",
				ErrNetworkPolicyNotEnforced, d.cfg.Namespace, enforceTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// waitCanaryReachable waits for the canary to be Running with an IP and then
// for a dial to actually succeed — the pod being Running says nothing about the
// listener being up, and this is the control the deny half is measured against.
func (d *Driver) waitCanaryReachable(ctx context.Context, name string) (string, error) {
	pods := d.client.CoreV1().Pods(d.cfg.Namespace)
	deadline := time.Now().Add(canaryReadyTimeout)
	var last string
	for {
		pod, err := pods.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", fmt.Errorf("netpol probe: get canary: %w", err)
		}
		switch {
		case terminal(pod.Status.Phase):
			return "", fmt.Errorf("netpol probe: canary pod ended in phase %s: %s", pod.Status.Phase, terminationReason(pod))
		case pod.Status.Phase == corev1.PodRunning && pod.Status.PodIP != "":
			addr := net.JoinHostPort(pod.Status.PodIP, strconv.FormatUint(uint64(canaryPort), 10))
			if err := d.tryDial(ctx, addr); err == nil {
				return addr, nil
			} else {
				last = "dial " + addr + ": " + err.Error()
			}
		default:
			last = "phase " + string(pod.Status.Phase) + " " + waitingReason(pod)
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("netpol probe: the canary pod was never reachable even with an allow policy (%s). "+
				"Either the canary image %q cannot be pulled/started here (set PROTEOS_K8S_NETPOL_PROBE_IMAGE), "+
				"or pod-to-pod traffic from the cluster-agent is blocked outright — in which case machines would be unreachable too",
				last, d.canaryImage())
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// tryDial is one bounded connection attempt to the canary through the same
// dialer DialGuest uses.
func (d *Driver) tryDial(ctx context.Context, addr string) error {
	ctx, cancel := context.WithTimeout(ctx, canaryDialTimeout)
	defer cancel()
	conn, err := d.dial(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return conn.Close()
}

// cleanupCanary deletes the canary pod and policy with a background context, so
// a cancelled preflight still leaves the namespace clean.
func (d *Driver) cleanupCanary(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	zero := int64(0)
	if err := d.client.CoreV1().Pods(d.cfg.Namespace).Delete(ctx, name, metav1.DeleteOptions{GracePeriodSeconds: &zero}); err != nil && !apierrors.IsNotFound(err) {
		slog.Warn("kubernetes: could not delete the netpol canary pod", "pod", name, "err", err)
	}
	if err := d.client.NetworkingV1().NetworkPolicies(d.cfg.Namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		slog.Warn("kubernetes: could not delete the netpol canary policy", "policy", name, "err", err)
	}
}

func (d *Driver) canaryImage() string {
	if d.cfg.NetpolProbeImage != "" {
		return d.cfg.NetpolProbeImage
	}
	return DefaultNetpolProbeImage
}

func canaryMeta(namespace, name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:      name,
		Namespace: namespace,
		Labels: map[string]string{
			ManagedByLabel: managedByValue,
			ComponentLabel: canaryComponent,
			canaryLabel:    name,
		},
	}
}

// canaryPod is a minimal, restricted-profile listener on the terminal port. It
// deliberately takes no RuntimeClass — the question is about the CNI, and a
// sandboxed runtime only adds start-up time — but it does take the machines'
// node selector and pull secret, so it lands where machines land.
func (d *Driver) canaryPod(name string) *corev1.Pod {
	tiny := corev1.ResourceList{
		corev1.ResourceCPU:    *resource.NewMilliQuantity(50, resource.DecimalSI),
		corev1.ResourceMemory: *resource.NewQuantity(64*1024*1024, resource.BinarySI),
	}
	pod := &corev1.Pod{
		ObjectMeta: canaryMeta(d.cfg.Namespace, name),
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: ptr(false),
			EnableServiceLinks:           ptr(false),
			NodeSelector:                 d.cfg.NodeSelector,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   ptr(true),
				RunAsUser:      ptr(int64(65532)),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:            "canary",
				Image:           d.canaryImage(),
				ImagePullPolicy: corev1.PullIfNotPresent,
				Args:            []string{"netexec", "--http-port=" + strconv.FormatUint(uint64(canaryPort), 10)},
				Ports:           []corev1.ContainerPort{{Name: "canary", ContainerPort: int32(canaryPort)}},
				Resources:       corev1.ResourceRequirements{Requests: tiny, Limits: tiny},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: ptr(false),
					ReadOnlyRootFilesystem:   ptr(true),
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
			}},
		},
	}
	if d.cfg.ImagePullSecret != "" {
		pod.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: d.cfg.ImagePullSecret}}
	}
	return pod
}

// canaryPolicy selects the canary and either permits the cluster-agent on the
// canary port (allow=true) or, with PolicyTypeIngress set and no rules, denies
// every ingress peer.
func (d *Driver) canaryPolicy(name string, allow bool) *networkingv1.NetworkPolicy {
	pol := &networkingv1.NetworkPolicy{
		ObjectMeta: canaryMeta(d.cfg.Namespace, name),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{canaryLabel: name}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		},
	}
	if allow {
		tcp := corev1TCP()
		port := intstr.FromInt32(int32(canaryPort))
		pol.Spec.Ingress = []networkingv1.NetworkPolicyIngressRule{{
			From: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{namespaceNameLabel: d.cfg.AgentNamespace},
				},
				PodSelector: &metav1.LabelSelector{MatchLabels: d.cfg.AgentPodLabels},
			}},
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}},
		}}
	}
	return pol
}

// waitingReason is the kubelet's explanation for a pod that is not Running yet
// (ImagePullBackOff, ContainerCreating, ...), for the probe's error message.
func waitingReason(pod *corev1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			return cs.State.Waiting.Reason
		}
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status != corev1.ConditionTrue && c.Message != "" {
			return c.Message
		}
	}
	return ""
}
