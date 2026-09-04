package kubernetes

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/tavon-ai/proteos/nodeagent/internal/driver"
)

// Capacity synthesizes a "host" shape for a whole cluster, so the control
// plane's scheduler (which ranks fixed hosts) can rank a cluster alongside
// Firecracker boxes without knowing the difference.
//
// Total is the sum of allocatable cpu/memory over the schedulable nodes that
// match the machine node selector; Used is the sum of what this driver's own
// machines request. That is deliberately an approximation: it ignores
// fragmentation (four free cores spread over four nodes will not fit a 4-vcpu
// machine) and it ignores everything else running on those nodes. It only has
// to be good enough to RANK hosts — Kubernetes, not the control plane, decides
// whether a pod actually fits, and a machine that cannot be placed stays
// Pending, which the driver reports as creating.
//
// Disk is not derived from nodes at all: machine volumes come from a
// StorageClass whose backing capacity the API server does not expose. Total
// disk is reported as zero, meaning "unknown/unbounded" — with a single active
// host the scheduler short-circuits before it looks, and an operator who needs
// a real bound sets a ResourceQuota on the namespace.
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

	nodes, err := d.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: labels.Set(d.cfg.NodeSelector).String(),
	})
	if err != nil {
		return driver.Capacity{}, fmt.Errorf("list nodes: %w", err)
	}

	var cap driver.Capacity
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if !schedulable(node) {
			continue
		}
		cap.TotalVcpus += int(node.Status.Allocatable.Cpu().Value())
		cap.TotalMemMiB += int(node.Status.Allocatable.Memory().Value() / (1024 * 1024))
	}

	pods, err := d.client.CoreV1().Pods(d.cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: ownedSelector(),
	})
	if err != nil {
		return driver.Capacity{}, fmt.Errorf("list machine pods: %w", err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if terminal(pod.Status.Phase) {
			continue
		}
		for j := range pod.Spec.Containers {
			req := pod.Spec.Containers[j].Resources.Requests
			cap.UsedVcpus += int(req.Cpu().Value())
			cap.UsedMemMiB += int(req.Memory().Value() / (1024 * 1024))
		}
	}

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
