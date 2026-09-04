package kubernetes

import (
	"context"
	"fmt"
	"log/slog"

	authv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// NewClient builds the clientset the driver runs against. An empty kubeconfig
// path means in-cluster (the production shape: the cluster-agent is a Deployment
// in the target cluster, using its ServiceAccount token); a path is the
// out-of-cluster developer/CI shape.
func NewClient(kubeconfig string) (kubernetes.Interface, error) {
	var (
		cfg *rest.Config
		err error
	)
	if kubeconfig == "" {
		cfg, err = rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("in-cluster config (set PROTEOS_K8S_KUBECONFIG to run outside a cluster): %w", err)
		}
	} else {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("kubeconfig %s: %w", kubeconfig, err)
		}
	}
	cfg.UserAgent = "proteos-node-agent"
	return kubernetes.NewForConfig(cfg)
}

// requiredVerbs is what the cluster-agent's Role must grant in the machine
// namespace for the driver to function at all.
var requiredVerbs = []struct{ resource, verb string }{
	{"pods", "get"}, {"pods", "list"}, {"pods", "create"}, {"pods", "delete"},
	{"persistentvolumeclaims", "get"}, {"persistentvolumeclaims", "list"},
	{"persistentvolumeclaims", "create"}, {"persistentvolumeclaims", "delete"},
	{"networkpolicies", "get"}, {"networkpolicies", "list"},
	{"networkpolicies", "create"}, {"networkpolicies", "update"}, {"networkpolicies", "delete"},
}

// Preflight fails fast on the misconfigurations that would otherwise surface as
// a confusing per-machine error hours later: a missing namespace, a Role that
// does not actually grant what the driver needs, or a RuntimeClass that was
// named but never installed.
//
// It deliberately does NOT verify that the CNI enforces NetworkPolicy — that
// needs a live canary pod and belongs to the deployment story (K2). What it can
// check cheaply, it checks at startup.
func (d *Driver) Preflight(ctx context.Context) error {
	if _, err := d.client.CoreV1().Namespaces().Get(ctx, d.cfg.Namespace, metav1.GetOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("namespace %q does not exist (apply deploy/kubernetes/)", d.cfg.Namespace)
		}
		// A Role scoped to one namespace cannot read the namespace object
		// itself; that is a normal, correct setup, so this is not fatal.
		slog.Debug("kubernetes: could not read the machine namespace", "err", err)
	}

	var missing []string
	for _, want := range requiredVerbs {
		allowed, err := d.canI(ctx, want.resource, want.verb)
		if err != nil {
			// SelfSubjectAccessReview itself may be denied; do not turn an
			// unverifiable permission into a startup failure.
			slog.Warn("kubernetes: RBAC self-check unavailable; skipping preflight", "err", err)
			missing = nil
			break
		}
		if !allowed {
			missing = append(missing, want.verb+" "+want.resource)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("service account lacks permissions in namespace %s: %v "+
			"(apply the Role in deploy/kubernetes/)", d.cfg.Namespace, missing)
	}

	if d.cfg.RuntimeClass == "" {
		slog.Warn("kubernetes: NO RuntimeClass configured — machine pods will run on the node's " +
			"default runtime and share its kernel. This is an explicit downgrade of the isolation " +
			"ProteOS normally provides with Firecracker; set PROTEOS_K8S_RUNTIME_CLASS=gvisor (or " +
			"kata) unless you have accepted that trade-off. See SECURITY.md.")
	} else if _, err := d.client.NodeV1().RuntimeClasses().
		Get(ctx, d.cfg.RuntimeClass, metav1.GetOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("RuntimeClass %q is not installed in this cluster: every machine pod "+
				"would stay Pending (install gVisor/Kata, or set PROTEOS_K8S_RUNTIME_CLASS= to "+
				"accept the downgrade)", d.cfg.RuntimeClass)
		}
		slog.Warn("kubernetes: could not verify the RuntimeClass", "runtime_class", d.cfg.RuntimeClass, "err", err)
	}

	if len(d.cfg.AgentPodLabels) == 0 {
		slog.Warn("kubernetes: PROTEOS_K8S_AGENT_POD_LABELS is unset, so machine NetworkPolicies " +
			"cannot name the cluster-agent as their ingress peer. Machines will be created with " +
			"ingress DENIED and will be unreachable. Set it to the cluster-agent Deployment's pod labels.")
	}
	return nil
}

// canI asks the API server whether this service account may perform verb on
// resource in the machine namespace.
func (d *Driver) canI(ctx context.Context, resource, verb string) (bool, error) {
	group := ""
	if resource == "networkpolicies" {
		group = "networking.k8s.io"
	}
	review := &authv1.SelfSubjectAccessReview{
		Spec: authv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authv1.ResourceAttributes{
				Namespace: d.cfg.Namespace,
				Group:     group,
				Resource:  resource,
				Verb:      verb,
			},
		},
	}
	got, err := d.client.AuthorizationV1().SelfSubjectAccessReviews().
		Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		return false, err
	}
	return got.Status.Allowed, nil
}
