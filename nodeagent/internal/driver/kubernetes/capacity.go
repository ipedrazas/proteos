package kubernetes

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/ipedrazas/proteos/nodeagent/internal/driver"
)

// Capacity synthesizes a "host" shape for a whole cluster, so the control
// plane's scheduler (which ranks fixed hosts) can rank a cluster alongside
// Firecracker boxes without knowing the difference.
//
// Total is the sum of allocatable cpu/memory over the schedulable nodes that
// match the machine node selector; Used is the sum of the shapes this driver's
// own machines were created with. That is deliberately an approximation: it ignores
// fragmentation (four free cores spread over four nodes will not fit a 4-vcpu
// machine) and it ignores everything else running on those nodes. It only has
// to be good enough to RANK hosts — Kubernetes, not the control plane, decides
// whether a pod actually fits, and a machine that cannot be placed stays
// Pending, which the driver reports as creating.
//
// Disk is not derived from nodes at all: machine volumes come from a
// StorageClass whose backing capacity the API server does not expose. Total
// disk is reported as zero, meaning "unknown/unbounded" — with a single active
// host the scheduler short-circuits before it looks — unless a ResourceQuota
// bounds requests.storage, in which case that is the figure.
//
// A ResourceQuota on the machine namespace is the operator's own statement of
// how much of the cluster machines may have (K6). When one exists it CAPS the
// node sum: nodes say what is physically present, the quota says what may be
// used, and the control plane should never be told more than the smaller. With
// CapacityFromQuota set the quota IS the total and the nodes are not consulted
// at all: on an autoscaled pool the nodes present are what the pool has grown
// to, not what it can grow to, and a pool scaled to zero would otherwise
// advertise nothing and never be given the machine that makes it grow.
//
// The answer is cached (CapacityTTL, 15s by default): the poller asks often,
// the node list changes rarely, and a full node listing on every poll is a
// pointless load on the API server.
func (d *Driver) Capacity(ctx context.Context) (driver.Capacity, error) {
	d.mu.Lock()
	if !d.capAt.IsZero() && time.Since(d.capAt) < d.cfg.CapacityTTL {
		cached := d.capCache
		d.mu.Unlock()
		return cached, nil
	}
	d.mu.Unlock()

	var cap driver.Capacity
	quota, err := d.quotaCeiling(ctx)
	if err != nil {
		return driver.Capacity{}, err
	}

	if d.cfg.CapacityFromQuota && quota.found {
		cap.TotalVcpus, cap.TotalMemMiB, cap.TotalDiskMiB = quota.vcpus, quota.memMiB, quota.diskMiB
	} else {
		if d.cfg.CapacityFromQuota {
			slog.Warn("kubernetes: PROTEOS_K8S_CAPACITY_FROM_QUOTA is set but the machine namespace has no " +
				"ResourceQuota with compute limits; reporting the nodes present instead")
		}
		nodes, err := d.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{
			LabelSelector: labels.Set(d.cfg.NodeSelector).String(),
		})
		if err != nil {
			return driver.Capacity{}, fmt.Errorf("list nodes: %w", err)
		}
		for i := range nodes.Items {
			node := &nodes.Items[i]
			if !schedulable(node) {
				continue
			}
			cap.TotalVcpus += int(node.Status.Allocatable.Cpu().Value())
			cap.TotalMemMiB += int(node.Status.Allocatable.Memory().Value() / (1024 * 1024))
		}
		if quota.found {
			cap.TotalVcpus = cappedBy(cap.TotalVcpus, quota.vcpus)
			cap.TotalMemMiB = cappedBy(cap.TotalMemMiB, quota.memMiB)
			cap.TotalDiskMiB = quota.diskMiB
		}
	}

	pods, err := d.client.CoreV1().Pods(d.cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: ownedSelector(),
	})
	if err != nil {
		return driver.Capacity{}, fmt.Errorf("list machine pods: %w", err)
	}
	// Used is the sum of the machines' DECLARED shapes, read back off the pod
	// annotations, not of their Kubernetes resource requests.
	//
	// A request is how this driver gets a machine scheduled, and it is
	// deliberately not the machine's shape: CPURequestRatio scales it down so a
	// machine fits a small node, and the sshd sidecar adds a fixed slice of its
	// own. Summing requests therefore answers a question the control plane never
	// asked. It reported a 1-vcpu machine as using 2 (Quantity.Value() rounds a
	// fractional core up, so the sidecar's 50m counted as a whole one) and then,
	// once that was fixed by accumulating milli, as using 0 on a cluster
	// configured to oversubscribe. Both are wrong in the way that matters: the
	// control plane is asking how much of this cluster its machines have been
	// promised, and that is what it asked for when it created them.
	//
	// A pod without readable annotations falls back to its requests, so a
	// hand-made or half-migrated pod still counts for something.
	var fallbackMilliCPU int64
	for i := range pods.Items {
		pod := &pods.Items[i]
		if terminal(pod.Status.Phase) {
			continue
		}
		if vcpus, memMiB, ok := declaredShape(pod); ok {
			cap.UsedVcpus += vcpus
			cap.UsedMemMiB += memMiB
			continue
		}
		for j := range pod.Spec.Containers {
			req := pod.Spec.Containers[j].Resources.Requests
			fallbackMilliCPU += req.Cpu().MilliValue()
			cap.UsedMemMiB += int(req.Memory().Value() / (1024 * 1024))
		}
	}
	cap.UsedVcpus += int((fallbackMilliCPU + 500) / 1000)

	// Disk: a machine holds its PVC until Destroy, exactly like the firecracker
	// driver's volume file, so every claim counts — running or not.
	pvcs, err := d.client.CoreV1().PersistentVolumeClaims(d.cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: ownedSelector(),
	})
	if err != nil {
		return driver.Capacity{}, fmt.Errorf("list machine pvcs: %w", err)
	}
	for i := range pvcs.Items {
		q := pvcs.Items[i].Spec.Resources.Requests[corev1.ResourceStorage]
		cap.UsedDiskMiB += int(q.Value() / (1024 * 1024))
	}

	d.mu.Lock()
	d.capCache, d.capAt = cap, time.Now()
	d.mu.Unlock()
	return cap, nil
}

// quotaCeiling is what the machine namespace's ResourceQuotas allow in the
// terms the control plane reasons in. found is false when no quota bounds
// compute at all, in which case the zero values mean nothing.
type quotaCeiling struct {
	found                  bool
	vcpus, memMiB, diskMiB int
}

// quotaCeiling reads every ResourceQuota in the machine namespace and folds
// them into one ceiling, taking the smallest bound per resource when several
// quotas apply (which is how the API server enforces them).
//
// vcpus come from limits.cpu, because a machine's vcpu count IS its CPU limit
// (its request is the limit scaled by CPURequestRatio, which the control plane
// does not know about); requests.cpu is used only when limits.cpu is absent.
// Memory is request == limit per machine, so either bound is the same figure;
// disk is requests.storage. A quota that names none of these is not a compute
// quota and is ignored.
//
// A Role that cannot list quotas (one that predates this) is not an error: the
// preflight warns once, and Capacity behaves as if there were no quota.
func (d *Driver) quotaCeiling(ctx context.Context) (quotaCeiling, error) {
	quotas, err := d.client.CoreV1().ResourceQuotas(d.cfg.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsForbidden(err) {
			return quotaCeiling{}, nil
		}
		return quotaCeiling{}, fmt.Errorf("list resourcequotas: %w", err)
	}

	var c quotaCeiling
	for i := range quotas.Items {
		hard := quotas.Items[i].Spec.Hard
		if q, ok := firstQuantity(hard, corev1.ResourceLimitsCPU, corev1.ResourceRequestsCPU, corev1.ResourceCPU); ok {
			c.found = true
			c.vcpus = minPositive(c.vcpus, int(q.MilliValue()/1000))
		}
		if q, ok := firstQuantity(hard, corev1.ResourceLimitsMemory, corev1.ResourceRequestsMemory, corev1.ResourceMemory); ok {
			c.found = true
			c.memMiB = minPositive(c.memMiB, int(q.Value()/(1024*1024)))
		}
		if q, ok := firstQuantity(hard, corev1.ResourceRequestsStorage); ok {
			c.found = true
			c.diskMiB = minPositive(c.diskMiB, int(q.Value()/(1024*1024)))
		}
	}
	return c, nil
}

// firstQuantity returns the first of the named resources present in a list.
func firstQuantity(list corev1.ResourceList, names ...corev1.ResourceName) (resource.Quantity, bool) {
	for _, n := range names {
		if q, ok := list[n]; ok {
			return q, true
		}
	}
	return resource.Quantity{}, false
}

// cappedBy lowers a node-derived total to a quota bound when the quota names
// one. A zero bound is a quota that does not bound this resource, not a bound
// of zero; a zero total is a pool with no nodes, and stays zero.
func cappedBy(total, bound int) int {
	if bound > 0 && bound < total {
		return bound
	}
	return total
}

// minPositive folds several quotas' bounds for one resource: the smallest
// that is set. Zero means "this quota does not bound it" and never wins.
func minPositive(a, b int) int {
	switch {
	case a <= 0:
		return b
	case b <= 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
}

// preflightQuota reports, at startup, how Capacity will answer: from a quota,
// from the nodes capped by one, or from the nodes alone — and warns when the
// operator asked for a quota the Role cannot read or the namespace does not
// have. None of it is fatal; the figure only ranks hosts.
func (d *Driver) preflightQuota(ctx context.Context) {
	_, err := d.client.CoreV1().ResourceQuotas(d.cfg.Namespace).List(ctx, metav1.ListOptions{})
	if apierrors.IsForbidden(err) {
		slog.Warn("kubernetes: the service account may not list resourcequotas in the machine namespace; "+
			"capacity will come from the nodes alone (re-apply the Role in deploy/kubernetes/ to let a quota cap it)",
			"namespace", d.cfg.Namespace)
		return
	}
	if err != nil {
		slog.Warn("kubernetes: could not read resourcequotas", "err", err)
		return
	}
	q, err := d.quotaCeiling(ctx)
	if err != nil {
		return
	}
	switch {
	case d.cfg.CapacityFromQuota && !q.found:
		slog.Warn("kubernetes: PROTEOS_K8S_CAPACITY_FROM_QUOTA is set but no ResourceQuota in the machine namespace " +
			"bounds cpu/memory — apply one (see deploy/kubernetes/base/quota-example.yaml), or capacity is the nodes present")
	case d.cfg.CapacityFromQuota:
		slog.Info("kubernetes: capacity is the machine namespace's ResourceQuota, not the nodes present (autoscaled pool)",
			"vcpus", q.vcpus, "mem_mib", q.memMiB, "disk_mib", q.diskMiB)
	case q.found:
		slog.Info("kubernetes: a ResourceQuota caps the advertised capacity",
			"vcpus", q.vcpus, "mem_mib", q.memMiB, "disk_mib", q.diskMiB)
	}
}

// declaredShape reads the vcpus/memory the control plane asked for off a machine
// pod's annotations — the same values EnsureRunning wrote there, which is why
// this driver needs no local state. Reports ok=false unless both are present and
// positive, so a malformed annotation falls back to requests rather than
// silently counting a machine as zero.
func declaredShape(pod *corev1.Pod) (vcpus, memMiB int, ok bool) {
	vcpus, err := strconv.Atoi(pod.Annotations[VcpusAnnotation])
	if err != nil || vcpus <= 0 {
		return 0, 0, false
	}
	memMiB, err = strconv.Atoi(pod.Annotations[MemMiBAnnotation])
	if err != nil || memMiB <= 0 {
		return 0, 0, false
	}
	return vcpus, memMiB, true
}

// schedulable rejects cordoned nodes and nodes the kubelet has not reported
// Ready for, so draining a node does not briefly inflate the cluster's
// advertised capacity.
func schedulable(node *corev1.Node) bool {
	if node.Spec.Unschedulable {
		return false
	}
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
