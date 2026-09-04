package kubernetes

import (
	"context"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	api "github.com/tavon-ai/proteos/nodeagent/api"
	"github.com/tavon-ai/proteos/nodeagent/internal/driver"
)

// objectMeta is the labels/annotations every object for one machine carries.
// The labels are for selection (List, capacity accounting, the NetworkPolicy's
// podSelector); the annotations carry the desired spec, which is why this
// driver needs no local state file.
func (d *Driver) objectMeta(name string, spec driver.VMSpec) metav1.ObjectMeta {
	ann := map[string]string{
		MachineIDAnnotation: spec.MachineID,
		VcpusAnnotation:     strconv.Itoa(spec.Vcpus),
		MemMiBAnnotation:    strconv.Itoa(spec.MemMiB),
		DiskMiBAnnotation:   strconv.Itoa(diskMiB(spec)),
	}
	if id, ok := diskID(spec); ok {
		ann[DiskIDAnnotation] = id
	}
	if spec.ImageRef != "" {
		ann[ImageRefAnnotation] = spec.ImageRef
	}
	if mode := spec.NetworkPolicy.Mode; mode != "" {
		ann[NetPolAnnotation] = mode
	}
	return metav1.ObjectMeta{
		Name:      name,
		Namespace: d.cfg.Namespace,
		Labels: map[string]string{
			ManagedByLabel: managedByValue,
			ComponentLabel: componentValue,
			MachineLabel:   resourceName(spec.MachineID),
		},
		Annotations: ann,
	}
}

// diskMiB is the machine's persistent volume size, falling back to a default
// when the control plane sent no disk: a pod's rootfs is ephemeral, so there is
// always a volume, unlike on Firecracker where a machine can run without one.
func diskMiB(spec driver.VMSpec) int {
	if len(spec.Disks) > 0 && spec.Disks[0].SizeMiB > 0 {
		return spec.Disks[0].SizeMiB
	}
	return defaultDiskMiB
}

func diskID(spec driver.VMSpec) (string, bool) {
	if len(spec.Disks) > 0 && spec.Disks[0].ID != "" {
		return spec.Disks[0].ID, true
	}
	return "", false
}

// ensurePVC creates the machine's persistent volume claim if absent. An existing
// claim is left alone: a PVC's size can only grow, and silently resizing a
// user's volume on an ordinary ensure is not something the control plane asked
// for. Growing a machine's disk is a separate operation.
func (d *Driver) ensurePVC(ctx context.Context, spec driver.VMSpec) error {
	name := pvcName(spec.MachineID)
	pvcs := d.client.CoreV1().PersistentVolumeClaims(d.cfg.Namespace)

	if _, err := pvcs.Get(ctx, name, metav1.GetOptions{}); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get pvc %s: %w", name, err)
	}

	meta := d.objectMeta(name, spec)
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: meta,
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: *resource.NewQuantity(int64(diskMiB(spec))*1024*1024, resource.BinarySI),
				},
			},
		},
	}
	if d.cfg.StorageClass != "" {
		sc := d.cfg.StorageClass
		pvc.Spec.StorageClassName = &sc
	}
	if _, err := pvcs.Create(ctx, pvc, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create pvc %s: %w", name, err)
	}
	return nil
}

// buildPod is the machine pod spec.
//
// Security context (K-plan decision 6): the guest keeps its root-agent /
// non-root-`dev` split, so the container runs as root — but as root INSIDE a
// sandboxed runtime, with privilege escalation off, the default seccomp profile
// on, and only the five capabilities the guest-agent actually uses (it chowns
// the tmpfs env dir and the persist tree to the session user, and spawns PTYs as
// that user). The sandbox, not the capability set, is what makes this safe;
// RuntimeClass is a precondition of the backend, not a hardening option.
func (d *Driver) buildPod(spec driver.VMSpec, image string) *corev1.Pod {
	name := resourceName(spec.MachineID)
	meta := d.objectMeta(name, spec)

	// A pod's rootfs is ephemeral, so /persist is the PVC and /run/proteos is
	// memory-backed: the secrets package requires its env dir to be tmpfs so
	// injected provider credentials never touch a disk.
	volumes := []corev1.Volume{
		{
			Name: persistVolume,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: pvcName(spec.MachineID),
				},
			},
		},
		{
			Name: runVolume,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory},
			},
		},
	}

	grace := stopGracePeriod
	pod := &corev1.Pod{
		ObjectMeta: meta,
		Spec: corev1.PodSpec{
			// Always: the kubelet supervises the guest-agent the way systemd
			// does inside the ext4 image. A crash restarts the container and
			// keeps the pod (and its IP), rather than tearing the machine down.
			RestartPolicy:                 corev1.RestartPolicyAlways,
			TerminationGracePeriodSeconds: &grace,
			AutomountServiceAccountToken:  ptr(false), // a machine is not a cluster client
			EnableServiceLinks:            ptr(false), // do not leak service env vars into the guest
			NodeSelector:                  d.cfg.NodeSelector,
			Volumes:                       volumes,
			Containers: []corev1.Container{{
				Name:            guestContainer,
				Image:           image,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Ports: []corev1.ContainerPort{
					{Name: "terminal", ContainerPort: int32(api.GuestTerminalPort)},
					{Name: "web", ContainerPort: int32(api.GuestWebPort)},
					{Name: "preview", ContainerPort: int32(api.GuestPreviewPort)},
				},
				Env: guestEnv(),
				VolumeMounts: []corev1.VolumeMount{
					{Name: persistVolume, MountPath: persistMountPath},
					{Name: runVolume, MountPath: runMountPath},
				},
				Resources: resourceRequirements(spec),
				// Readiness gates DialGuest: the driver refuses to tunnel to a
				// pod whose guest-agent is not answering /healthz yet, which is
				// what keeps a "running" machine actually attachable.
				ReadinessProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{
						HTTPGet: &corev1.HTTPGetAction{
							Path: "/healthz",
							Port: intstr.FromInt32(int32(api.GuestTerminalPort)),
						},
					},
					InitialDelaySeconds: 1,
					PeriodSeconds:       2,
					FailureThreshold:    30,
				},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: ptr(false),
					Privileged:               ptr(false),
					ReadOnlyRootFilesystem:   ptr(false), // the guest is a dev box; it writes everywhere
					Capabilities: &corev1.Capabilities{
						Drop: []corev1.Capability{"ALL"},
						Add: []corev1.Capability{
							"CHOWN", "FOWNER", "DAC_OVERRIDE", "SETUID", "SETGID",
						},
					},
					SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				},
			}},
		},
	}

	if d.cfg.RuntimeClass != "" {
		rc := d.cfg.RuntimeClass
		pod.Spec.RuntimeClassName = &rc
	}
	if d.cfg.ImagePullSecret != "" {
		pod.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: d.cfg.ImagePullSecret}}
	}
	return pod
}

// guestEnv sets the guest-agent's listeners and paths explicitly rather than
// relying on the image's ENV, so the driver and the image cannot drift apart on
// which ports the pod IP is dialed at.
func guestEnv() []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "PROTEOS_GUEST_LISTEN", Value: listenSpec(api.GuestTerminalPort)},
		{Name: "PROTEOS_GUEST_WEB_LISTEN", Value: listenSpec(api.GuestWebPort)},
		{Name: "PROTEOS_GUEST_PREVIEW_LISTEN", Value: listenSpec(api.GuestPreviewPort)},
		{Name: "PROTEOS_GUEST_PERSIST", Value: persistMountPath},
		{Name: "PROTEOS_GUEST_ENV_DIR", Value: runMountPath + "/env"},
	}
}

func listenSpec(port uint32) string {
	return "tcp:0.0.0.0:" + strconv.FormatUint(uint64(port), 10)
}

// resourceRequirements maps the machine's vcpu/memory shape onto pod resources.
// Requests and limits are equal so a machine gets the Guaranteed QoS class: the
// firecracker backend hands a VM a fixed slice of the host, and a machine that
// is silently throttled or evicted under node pressure would be a worse
// experience than one the scheduler simply cannot place.
func resourceRequirements(spec driver.VMSpec) corev1.ResourceRequirements {
	list := corev1.ResourceList{}
	if spec.Vcpus > 0 {
		list[corev1.ResourceCPU] = *resource.NewQuantity(int64(spec.Vcpus), resource.DecimalSI)
	}
	if spec.MemMiB > 0 {
		list[corev1.ResourceMemory] = *resource.NewQuantity(int64(spec.MemMiB)*1024*1024, resource.BinarySI)
	}
	if len(list) == 0 {
		return corev1.ResourceRequirements{}
	}
	limits := corev1.ResourceList{}
	for k, v := range list {
		limits[k] = v
	}
	return corev1.ResourceRequirements{Requests: list, Limits: limits}
}

func ptr[T any](v T) *T { return &v }
