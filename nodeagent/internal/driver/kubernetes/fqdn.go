package kubernetes

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	api "github.com/tavon-ai/proteos/nodeagent/api"
	"github.com/tavon-ai/proteos/nodeagent/internal/driver"
)

// FQDN-aware egress (K6).
//
// A Kubernetes NetworkPolicy is IP-based, so the driver's allow_domains mode is
// a SNAPSHOT: the domains are resolved once, when the machine is ensured, and
// written as /32 ipBlocks (network.go). A domain whose address changes after
// that is not followed, a name behind a rotating CDN pool is only partly
// covered, and a wildcard cannot be expressed at all. That is the same
// limitation the firecracker driver's nft ip sets have, and it is the one this
// file lifts where the CNI can: Cilium's toFQDNs rules learn addresses from
// the DNS answers the machine itself receives, through a transparent DNS proxy,
// so the policy follows the name for as long as the answer's TTL says it may.
//
// The engine is additive. The snapshot NetworkPolicy is still written — Cilium
// unions every policy selecting a pod, and the snapshot keeps the machine
// working while the proxy has not yet seen a lookup — and a CiliumNetworkPolicy
// is written BESIDE it for allow_domains machines only:
//
//   - a DNS rule to the cluster resolver with an L7 `dns` section, which is what
//     turns the proxy on for this pod (without it Cilium never sees the answers
//     and toFQDNs matches nothing);
//   - one toFQDNs entry per domain: matchName for a plain name, matchPattern
//     for a `*.` wildcard.
//
// deny_domains is NOT lifted: Cilium's deny rules do not accept toFQDNs, and
// the open-source Calico has no DNS policy at all (it is an Enterprise
// feature). deny_domains therefore stays a snapshot on every CNI, and the
// operator docs say so.
//
// The CRD is reached through the dynamic client as unstructured objects rather
// than by importing Cilium's API module, whose dependency tree is most of
// Cilium. The object is small enough that a typed client buys nothing.

// cnpGVR is CiliumNetworkPolicy, namespaced.
var cnpGVR = schema.GroupVersionResource{Group: "cilium.io", Version: "v2", Resource: "ciliumnetworkpolicies"}

// cnpKind is the kind the dynamic client needs written into the object.
const cnpKind = "CiliumNetworkPolicy"

// defaultDNSPodLabels selects the cluster resolver's pods. Every distribution
// that ships CoreDNS labels it this way (kubeadm, kind, k3s, GKE, EKS), which is
// why the driver can name it without configuration; DNSPodLabels overrides it.
var defaultDNSPodLabels = map[string]string{"k8s-app": "kube-dns"}

func fqdnPolicyName(machineID string) string { return resourceName(machineID) }

// fqdnEnabled reports whether an FQDN engine is configured AND usable.
func (d *Driver) fqdnEnabled() bool {
	return d.cfg.FQDNPolicy == FQDNPolicyCilium && d.dyn != nil
}

// ensureFQDNPolicy writes, rewrites or removes the machine's CiliumNetworkPolicy
// to match its policy mode. Called on every ensure, after the NetworkPolicy:
// like it, the object has no identity a running machine depends on, so it is
// simply made to match.
//
// A no-op when no engine is configured. Turning the engine OFF later does not
// remove the objects it wrote (that would need the very permissions being given
// up); Destroy still removes a machine's, and the README gives the one-liner
// for the rest.
func (d *Driver) ensureFQDNPolicy(ctx context.Context, spec driver.VMSpec) error {
	if !d.fqdnEnabled() {
		return nil
	}
	name := fqdnPolicyName(spec.MachineID)
	client := d.dyn.Resource(cnpGVR).Namespace(d.cfg.Namespace)

	if spec.NetworkPolicy.Mode != api.NetworkPolicyAllowDomains {
		// Any other mode is fully expressed by the NetworkPolicy; a CNP left
		// over from a previous allow_domains would WIDEN it.
		return d.deleteFQDNPolicy(ctx, spec.MachineID)
	}

	desired := d.buildFQDNPolicy(spec)
	existing, err := client.Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		if _, err := client.Create(ctx, desired, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create ciliumnetworkpolicy %s: %w", name, err)
		}
		slog.Info("kubernetes: FQDN egress policy written", "machine", spec.MachineID,
			"engine", d.cfg.FQDNPolicy, "domains", spec.NetworkPolicy.Domains)
		return nil
	case err != nil:
		return fmt.Errorf("get ciliumnetworkpolicy %s: %w", name, err)
	}
	desired.SetResourceVersion(existing.GetResourceVersion())
	if _, err := client.Update(ctx, desired, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update ciliumnetworkpolicy %s: %w", name, err)
	}
	return nil
}

// deleteFQDNPolicy is Destroy's half (and the mode-change half of ensure).
// NotFound is success, and so is "no engine configured".
func (d *Driver) deleteFQDNPolicy(ctx context.Context, machineID string) error {
	if !d.fqdnEnabled() {
		return nil
	}
	name := fqdnPolicyName(machineID)
	err := d.dyn.Resource(cnpGVR).Namespace(d.cfg.Namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete ciliumnetworkpolicy %s: %w", name, err)
	}
	return nil
}

// buildFQDNPolicy renders the CiliumNetworkPolicy for an allow_domains machine.
func (d *Driver) buildFQDNPolicy(spec driver.VMSpec) *unstructured.Unstructured {
	meta := d.objectMeta(fqdnPolicyName(spec.MachineID), spec)

	dnsLabels := map[string]any{
		// Cilium label keys carry a source prefix; k8s: is the pod's own
		// Kubernetes labels plus the namespace pseudo-label.
		"k8s:io.kubernetes.pod.namespace": d.cfg.DNSNamespace,
	}
	for k, v := range d.dnsPodLabels() {
		dnsLabels["k8s:"+k] = v
	}

	fqdns := make([]any, 0, len(spec.NetworkPolicy.Domains))
	for _, domain := range spec.NetworkPolicy.Domains {
		domain = strings.ToLower(strings.TrimSpace(domain))
		if domain == "" {
			continue
		}
		if strings.HasPrefix(domain, "*.") {
			fqdns = append(fqdns, map[string]any{"matchPattern": domain})
		} else {
			fqdns = append(fqdns, map[string]any{"matchName": domain})
		}
	}

	labels := map[string]any{}
	for k, v := range meta.Labels {
		labels[k] = v
	}
	annotations := map[string]any{}
	for k, v := range meta.Annotations {
		annotations[k] = v
	}

	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": cnpGVR.Group + "/" + cnpGVR.Version,
		"kind":       cnpKind,
		"metadata": map[string]any{
			"name":        meta.Name,
			"namespace":   meta.Namespace,
			"labels":      labels,
			"annotations": annotations,
		},
		"spec": map[string]any{
			"endpointSelector": map[string]any{
				"matchLabels": map[string]any{MachineLabel: resourceName(spec.MachineID)},
			},
			"egress": []any{
				// The DNS proxy. `rules.dns` with a match-everything pattern
				// is what makes Cilium observe this pod's lookups; the ports
				// mirror the NetworkPolicy's DNS rule.
				map[string]any{
					"toEndpoints": []any{map[string]any{"matchLabels": dnsLabels}},
					"toPorts": []any{map[string]any{
						"ports": []any{map[string]any{"port": "53", "protocol": "ANY"}},
						"rules": map[string]any{"dns": []any{map[string]any{"matchPattern": "*"}}},
					}},
				},
				// The names themselves. No toPorts: any port to an address a
				// listed name resolved to, matching what the snapshot ipBlocks
				// permit.
				map[string]any{"toFQDNs": fqdns},
			},
		},
	}}
	return obj
}

// dnsPodLabels is the configured resolver selector or the default.
func (d *Driver) dnsPodLabels() map[string]string {
	if len(d.cfg.DNSPodLabels) > 0 {
		return d.cfg.DNSPodLabels
	}
	return defaultDNSPodLabels
}

// preflightFQDN verifies the configured engine can actually be used: the
// dynamic client exists, the CRD is served, and the Role grants the verbs. A
// misconfigured engine must be a startup error, not a per-machine ensure
// failure hours later — the same rule as every other preflight check.
func (d *Driver) preflightFQDN(ctx context.Context) error {
	switch d.cfg.FQDNPolicy {
	case "":
		return nil
	case FQDNPolicyCilium:
	default:
		return fmt.Errorf("PROTEOS_K8S_FQDN_POLICY=%q is not a known engine (want %q or empty)",
			d.cfg.FQDNPolicy, FQDNPolicyCilium)
	}
	if d.dyn == nil {
		return fmt.Errorf("PROTEOS_K8S_FQDN_POLICY=%s needs a dynamic client and the driver was built without one", d.cfg.FQDNPolicy)
	}

	resources, err := d.client.Discovery().ServerResourcesForGroupVersion(cnpGVR.GroupVersion().String())
	if err != nil || resources == nil {
		return fmt.Errorf("PROTEOS_K8S_FQDN_POLICY=cilium but the API server does not serve %s — "+
			"is Cilium installed? (%v). Unset it to fall back to the IP snapshot", cnpGVR.GroupVersion(), err)
	}
	found := false
	for _, r := range resources.APIResources {
		if r.Name == cnpGVR.Resource {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("PROTEOS_K8S_FQDN_POLICY=cilium but %s has no %s resource", cnpGVR.GroupVersion(), cnpGVR.Resource)
	}

	var missing []string
	for _, verb := range []string{"get", "create", "update", "delete"} {
		allowed, err := d.canIGroup(ctx, cnpGVR.Group, cnpGVR.Resource, verb)
		if err != nil {
			slog.Warn("kubernetes: RBAC self-check for ciliumnetworkpolicies unavailable", "err", err)
			return nil
		}
		if !allowed {
			missing = append(missing, verb)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("PROTEOS_K8S_FQDN_POLICY=cilium but the service account may not %v ciliumnetworkpolicies "+
			"in %s (re-apply the Role in deploy/kubernetes/)", missing, d.cfg.Namespace)
	}
	slog.Info("kubernetes: FQDN egress engine enabled — allow_domains follows names, not a boot-time snapshot",
		"engine", d.cfg.FQDNPolicy)
	return nil
}
