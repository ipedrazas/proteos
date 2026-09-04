package kubernetes

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	api "github.com/tavon-ai/proteos/nodeagent/api"
	"github.com/tavon-ai/proteos/nodeagent/internal/driver"
)

// Status maps a machine's pod (or its absence) onto a driver status.
//
// The three-way distinction the control plane depends on:
//
//	pod present            → its phase decides (see statusOf)
//	no pod, PVC present    → stopped: the machine exists, its volume is waiting
//	neither                → ErrUnknownMachine (404 to the control plane)
func (d *Driver) Status(ctx context.Context, machineID string) (driver.Status, error) {
	pod, err := d.client.CoreV1().Pods(d.cfg.Namespace).
		Get(ctx, resourceName(machineID), metav1.GetOptions{})
	switch {
	case err == nil && !terminating(pod.ObjectMeta):
		return d.statusOf(pod), nil
	case err != nil && !apierrors.IsNotFound(err):
		return driver.Status{}, fmt.Errorf("get pod %s: %w", resourceName(machineID), err)
	}

	// No live pod. The machine still exists if its volume does — that is the
	// difference between a stop and a destroy. A pod that is merely terminating
	// counts as gone: it has been told to die and cannot serve a session, and
	// the API server keeps it visible for the whole grace period.
	if !d.pvcExists(ctx, machineID) {
		return driver.Status{}, driver.ErrUnknownMachine
	}

	// A pod that is still terminating means the shutdown is in flight; without
	// this the machine would report "stopped" while its container is still
	// running.
	state := api.StateStopped
	if err == nil {
		state = api.StateStopping
	}
	return driver.Status{
		MachineID: machineID,
		State:     state,
		Handle:    d.handle(machineID),
		Boot:      api.BootCold,
		DiskID:    d.diskIDFromPVC(ctx, machineID),
	}, nil
}

// terminating reports whether an object has been deleted but is still visible:
// a pod inside its termination grace period, or a PVC whose pvc-protection
// finalizer is held while a pod still mounts it. Both look present to a Get and
// neither is usable, and neither ever happens against the fake clientset — which
// deletes instantly — so only a real cluster shows this up.
func terminating(meta metav1.ObjectMeta) bool {
	return meta.DeletionTimestamp != nil
}

// List returns every machine this driver owns in the namespace. Machines that
// are stopped (pod gone, PVC kept) are included from the PVC side, so the
// control plane's reconciliation sees them exactly as Status would report them.
func (d *Driver) List(ctx context.Context) ([]driver.Status, error) {
	pods, err := d.client.CoreV1().Pods(d.cfg.Namespace).
		List(ctx, metav1.ListOptions{LabelSelector: ownedSelector()})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}

	out := make([]driver.Status, 0, len(pods.Items))
	seen := make(map[string]struct{}, len(pods.Items))
	for i := range pods.Items {
		if terminating(pods.Items[i].ObjectMeta) {
			continue // dying: let the PVC below report it as stopped/gone
		}
		st := d.statusOf(&pods.Items[i])
		if st.MachineID == "" {
			continue // not ours after all: no machine-id annotation
		}
		seen[st.MachineID] = struct{}{}
		out = append(out, st)
	}

	pvcs, err := d.client.CoreV1().PersistentVolumeClaims(d.cfg.Namespace).
		List(ctx, metav1.ListOptions{LabelSelector: ownedSelector()})
	if err != nil {
		return nil, fmt.Errorf("list pvcs: %w", err)
	}
	for i := range pvcs.Items {
		pvc := &pvcs.Items[i]
		if terminating(pvc.ObjectMeta) {
			continue // destroyed; the volume is only still visible for its finalizer
		}
		id := pvc.Annotations[MachineIDAnnotation]
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		out = append(out, driver.Status{
			MachineID: id,
			State:     api.StateStopped,
			Handle:    d.handle(id),
			Boot:      api.BootCold,
			DiskID:    pvc.Annotations[DiskIDAnnotation],
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].MachineID < out[j].MachineID })
	return out, nil
}

// statusOf maps one pod onto a driver status.
//
// Boot is always cold and Snapshot always absent: a pod has no RAM to restore.
// The control plane already handles that — its poller deletes the snapshot row
// when the agent reports snapshot.present=false, and a machine simply boots
// cold.
func (d *Driver) statusOf(pod *corev1.Pod) driver.Status {
	id := pod.Annotations[MachineIDAnnotation]
	st := driver.Status{
		MachineID: id,
		Handle:    d.handle(id),
		GuestIP:   pod.Status.PodIP,
		Boot:      api.BootCold,
		DiskID:    pod.Annotations[DiskIDAnnotation],
	}
	st.State, st.Reason = phaseToState(pod)
	return st
}

// phaseToState is the pod-phase → agentapi-state mapping.
//
//	Pending                        → creating (scheduling, pulling, mounting)
//	Running + guest ready          → running
//	Running + guest crash-looping  → error (else a broken machine reports
//	                                 "creating" forever, since RestartPolicy
//	                                 Always keeps the pod in Running)
//	Running + not ready yet        → creating
//	Succeeded                      → stopped (the guest-agent exited cleanly)
//	Failed / Unknown               → error, with the container's own reason
func phaseToState(pod *corev1.Pod) (state, reason string) {
	switch pod.Status.Phase {
	case corev1.PodPending:
		return api.StateCreating, ""
	case corev1.PodSucceeded:
		return api.StateStopped, ""
	case corev1.PodFailed, corev1.PodUnknown:
		return api.StateError, terminationReason(pod)
	case corev1.PodRunning:
		if reason, looping := crashLoop(pod); looping {
			return api.StateError, reason
		}
		if guestReady(pod) {
			return api.StateRunning, ""
		}
		return api.StateCreating, ""
	default:
		return api.StateCreating, ""
	}
}

// guestReady reports whether the guest container passed its readiness probe, i.e.
// the guest-agent is answering /healthz. Pod-level Ready is not enough once K4
// adds an sshd sidecar, so the check is per-container.
func guestReady(pod *corev1.Pod) bool {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == guestContainer {
			return cs.Ready
		}
	}
	return false
}

// crashLoop reports whether the guest container is stuck restarting, and why.
func crashLoop(pod *corev1.Pod) (string, bool) {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != guestContainer {
			continue
		}
		if w := cs.State.Waiting; w != nil && w.Reason == "CrashLoopBackOff" {
			msg := w.Reason
			if t := cs.LastTerminationState.Terminated; t != nil && t.Reason != "" {
				msg = fmt.Sprintf("%s (last exit: %s, code %d)", w.Reason, t.Reason, t.ExitCode)
			}
			return msg, true
		}
	}
	return "", false
}

// terminationReason digs the most useful explanation out of a dead pod: the
// guest container's own termination reason if there is one, else the pod-level
// reason/message the kubelet or scheduler recorded (Evicted, OOMKilled, ...).
func terminationReason(pod *corev1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != guestContainer {
			continue
		}
		if t := cs.State.Terminated; t != nil {
			if t.Message != "" {
				return fmt.Sprintf("%s: %s", t.Reason, t.Message)
			}
			if t.Reason != "" {
				return fmt.Sprintf("%s (exit %d)", t.Reason, t.ExitCode)
			}
		}
	}
	if pod.Status.Reason != "" {
		if pod.Status.Message != "" {
			return fmt.Sprintf("%s: %s", pod.Status.Reason, pod.Status.Message)
		}
		return pod.Status.Reason
	}
	return string(pod.Status.Phase)
}

// diskIDFromPVC reads the disk id off a stopped machine's claim, so a stopped
// machine still reports the disk the control plane recorded at create.
func (d *Driver) diskIDFromPVC(ctx context.Context, machineID string) string {
	pvc, err := d.client.CoreV1().PersistentVolumeClaims(d.cfg.Namespace).
		Get(ctx, pvcName(machineID), metav1.GetOptions{})
	if err != nil {
		return ""
	}
	return pvc.Annotations[DiskIDAnnotation]
}
