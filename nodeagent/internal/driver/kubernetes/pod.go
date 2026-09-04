package kubernetes

import (
	"context"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
				Resources: d.resourceRequirements(spec),
				// Readiness gates DialGuest: the driver refuses to tunnel to a
				// pod whose guest-agent is not answering /healthz yet, which is
				// what keeps a "running" machine actually attachable.
				//
				// EXEC, not httpGet, and deliberately so. An httpGet probe is
				// performed by the kubelet from the NODE's address, which means
				// the machine's NetworkPolicy would have to permit a second
				// ingress peer on the very guest ports it exists to restrict to
				// exactly one — and the node's address is not something a
				// portable policy can name. Whether kubelet probe traffic is
				// even subject to policy differs between CNIs, so an httpGet
				// probe makes readiness a property of the CNI rather than of the
				// guest. An exec probe runs through the CRI rather than the
				// network: unaffected by policy, identical on every CNI, and it
				// keeps the guest's network surface exactly "the cluster-agent,
				// and nothing else".
				ReadinessProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{
						Exec: &corev1.ExecAction{Command: healthzProbeCommand()},
					},
					InitialDelaySeconds: 1,
					PeriodSeconds:       3,
					TimeoutSeconds:      3,
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
//
// MEMORY is always request == limit. Memory is incompressible: a machine that
// is overcommitted on memory is not slow, it is OOM-killed, and losing someone's
// agent session to a noisy neighbour is not a trade worth making.
//
// CPU is compressible, so its request is scaled by CPURequestRatio (default
// 1.0 — request == limit, the Guaranteed-QoS behaviour a Firecracker VM's fixed
// slice of a host most resembles). Lowering it lets a machine be SCHEDULED with
// less than a dedicated core while still being ALLOWED a full one when the node
// is idle. Without that knob a 2-vcpu machine demands two whole free cores, so
// the backend cannot place a single machine on a small cluster — a dev cluster,
// a single-node k3s, or a CI kind node — at all.
func (d *Driver) resourceRequirements(spec driver.VMSpec) corev1.ResourceRequirements {
	requests, limits := corev1.ResourceList{}, corev1.ResourceList{}

	if spec.Vcpus > 0 {
		limits[corev1.ResourceCPU] = *resource.NewQuantity(int64(spec.Vcpus), resource.DecimalSI)
		milli := int64(float64(spec.Vcpus) * 1000 * d.cpuRequestRatio())
		if milli < 1 {
			milli = 1 // never request zero: that is a different QoS class entirely
		}
		requests[corev1.ResourceCPU] = *resource.NewMilliQuantity(milli, resource.DecimalSI)
	}
	if spec.MemMiB > 0 {
		mem := *resource.NewQuantity(int64(spec.MemMiB)*1024*1024, resource.BinarySI)
		requests[corev1.ResourceMemory] = mem
		limits[corev1.ResourceMemory] = mem
	}

	if len(requests) == 0 {
		return corev1.ResourceRequirements{}
	}
	return corev1.ResourceRequirements{Requests: requests, Limits: limits}
}

// cpuRequestRatio is the configured ratio, clamped to (0,1]. A value outside
// that range is meaningless — above 1 would request more than the limit, which
// the API server rejects — so it falls back to the safe default rather than
// producing a pod that cannot be created.
func (d *Driver) cpuRequestRatio() float64 {
	r := d.cfg.CPURequestRatio
	if r <= 0 || r > 1 {
		return 1
	}
	return r
}

// healthzProbeCommand curls the guest-agent's own /healthz over loopback inside
// the container. curl is part of the guest image's platform layer (both the
// ext4 bake and the OCI build install it), and -m bounds it so a wedged agent
// fails the probe rather than hanging it.
func healthzProbeCommand() []string {
	return []string{
		"curl", "--fail", "--silent", "--show-error", "--max-time", "2",
		"http://127.0.0.1:" + strconv.FormatUint(uint64(api.GuestTerminalPort), 10) + "/healthz",
	}
}

func ptr[T any](v T) *T { return &v }
