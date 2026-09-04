// Package kubernetes implements a Driver that runs each machine as a Pod in a
// Kubernetes cluster instead of a Firecracker microVM (K-plan stage K1).
//
// The node-agent running this driver is deployed INSIDE the target cluster as a
// "cluster-agent" Deployment; to the control plane it is one more hosts row
// speaking the unchanged agentapi contract. Nothing above the driver seam
// changes: the guest tunnel still hands back a net.Conn and still never parses
// what flows through it.
//
// What a pod cannot honour natively, and what this driver does instead:
//
//   - Boot/Snapshot. Pods have no RAM restore, so Status always reports
//     BootCold with Snapshot.Present=false. The control plane already tolerates
//     "cold stop, no snapshot" (its poller deletes the snapshot row and boots
//     cold).
//   - StopModeHibernate. Collapses to "delete the pod, keep the PVC". Accepted
//     and logged as a downgrade until K3 teaches the control plane to send
//     poweroff for kubernetes hosts.
//   - VolumeKey. There is no host here to luksOpen on. Accepted and ignored in
//     v1 with a loud log line; encryption at rest is the StorageClass's job.
//     See SECURITY.md for the delta and K5 for the dm-crypt option.
//   - GuestIP / tap / MAC allocation. Meaningless: the driver never touches the
//     node-agent's local state.Store and uses the pod IP.
//   - Reattach. The API server IS the state, so re-attaching is a list, not a
//     recovery: pods that outlived the cluster-agent are still there, still
//     labelled, still reachable.
//
// The driver is therefore stateless: every answer comes from the API server,
// and the desired spec travels as pod annotations so List/Status/Reattach need
// no local records.
package kubernetes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/tavon-ai/proteos/nodeagent/internal/driver"
)

// Label keys. ManagedByLabel/ComponentLabel select everything this driver owns
// (List, Reattach, capacity accounting); MachineLabel carries the machine's
// resource name so a single machine's objects can be selected without a Get.
// The TRUE machine id lives in the MachineIDAnnotation, because a machine id is
// not guaranteed to be a valid label value.
const (
	ManagedByLabel = "app.kubernetes.io/managed-by"
	ComponentLabel = "app.kubernetes.io/component"
	MachineLabel   = "proteos.io/machine"

	managedByValue = "proteos"
	componentValue = "machine"
)

// Annotation keys. The desired spec is stored on the pod so the driver can
// answer Status/List without a local state file — the API server is the record.
const (
	MachineIDAnnotation = "proteos.io/machine-id"
	VcpusAnnotation     = "proteos.io/vcpus"
	MemMiBAnnotation    = "proteos.io/mem-mib"
	DiskIDAnnotation    = "proteos.io/disk-id"
	DiskMiBAnnotation   = "proteos.io/disk-mib"
	ImageRefAnnotation  = "proteos.io/image-ref"
	NetPolAnnotation    = "proteos.io/network-policy-mode"
)

// Container and volume names inside a machine pod.
const (
	guestContainer = "guest"
	persistVolume  = "persist"
	runVolume      = "run-proteos"

	persistMountPath = "/persist"
	runMountPath     = "/run/proteos"
)

// stopGracePeriod is how long the kubelet gives the guest-agent to shut down
// after SIGTERM. The agent's own graceful shutdown is 5s (it drains the HTTP
// server); the margin covers code-server and PTY children.
const stopGracePeriod int64 = 15

// defaultDiskMiB is the PVC size used when a machine arrives with no disk in its
// spec. A pod's rootfs is ephemeral, so the machine still needs somewhere for
// $HOME and the workspace to survive a stop.
const defaultDiskMiB = 10240

// Config is the driver's operator-supplied configuration, resolved from the
// environment by internal/config.
type Config struct {
	// Namespace is where machine pods, PVCs and NetworkPolicies are created.
	Namespace string

	// AgentNamespace is the namespace the cluster-agent itself runs in; it is
	// the ingress peer in every machine's NetworkPolicy. Defaults to Namespace.
	AgentNamespace string

	// AgentPodLabels selects the cluster-agent pod as the only permitted
	// ingress peer on the guest ports. Empty ⇒ the driver cannot write a
	// meaningful ingress restriction and says so loudly (see EnsureRunning).
	AgentPodLabels map[string]string

	// StorageClass for machine PVCs. Empty ⇒ the cluster default.
	StorageClass string

	// RuntimeClass for machine pods — gVisor or Kata. This is the whole
	// isolation story on this backend (K-plan decision 6): the guest runs
	// unauthenticated listeners and an agent with --dangerously-skip-permissions
	// on the assumption that it is sandboxed. An empty value is an explicit,
	// documented downgrade to the node's default runtime.
	RuntimeClass string

	// ImagePullSecret is attached to every machine pod when set.
	ImagePullSecret string

	// NodeSelector pins machine pods to a dedicated node pool.
	NodeSelector map[string]string

	// ClusterCIDRs are the pod/service CIDRs a machine may never reach. They
	// are added to the egress deny list alongside RFC1918, mirroring the
	// firecracker driver's nft forward drop.
	ClusterCIDRs []string

	// DNSNamespace holds the cluster DNS service; machine egress always allows
	// port 53 to it, or every policy mode would break name resolution.
	DNSNamespace string

	// DefaultImage is used when an EnsureRequest carries no image_ref. It exists
	// for the tracer bullet, before K3 teaches the control plane to send one.
	DefaultImage string

	// CPURequestRatio scales a machine's CPU *request* relative to its limit
	// (default 1.0 — request == limit). CPU is compressible, so lowering this
	// lets a machine be scheduled with less than a dedicated core per vcpu while
	// still being allowed a full one when the node is idle. Memory is never
	// scaled: an overcommitted machine is OOM-killed, not slowed.
	//
	// At 1.0 a 2-vcpu machine needs two whole free cores, which no small cluster
	// (a dev cluster, a single-node k3s, a CI kind node) can provide.
	CPURequestRatio float64

	// DialTimeout bounds DialGuest's TCP connect to the pod IP.
	DialTimeout time.Duration

	// CapacityTTL is how long a Capacity() answer is cached; the node list it
	// derives from changes on a much slower cadence than the poller asks.
	CapacityTTL time.Duration
}

// Driver runs machines as pods. It holds no per-machine state: every method is
// a call against the API server.
type Driver struct {
	client kubernetes.Interface
	cfg    Config

	mu       sync.Mutex
	capCache driver.Capacity
	capAt    time.Time
}

var (
	_ driver.Driver      = (*Driver)(nil)
	_ driver.GuestDialer = (*Driver)(nil)
)

// New builds a Driver against an already-constructed clientset. Tests pass
// k8s.io/client-go/kubernetes/fake; production passes a real in-cluster client
// (see NewClient).
func New(client kubernetes.Interface, cfg Config) *Driver {
	if cfg.Namespace == "" {
		cfg.Namespace = "proteos-machines"
	}
	if cfg.AgentNamespace == "" {
		cfg.AgentNamespace = cfg.Namespace
	}
	if cfg.DNSNamespace == "" {
		cfg.DNSNamespace = "kube-system"
	}
	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = 5 * time.Second
	}
	if cfg.CapacityTTL == 0 {
		cfg.CapacityTTL = 15 * time.Second
	}
	return &Driver{client: client, cfg: cfg}
}

// dns1123Label matches a name Kubernetes will accept for an object.
var dns1123Label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// resourceName maps a machine id onto the name of its pod (and, with suffixes,
// its PVC and NetworkPolicy). Machine ids are UUIDs today, which are already
// valid DNS-1123 labels, so the common case reads as "m-<machine id>" in
// kubectl. Anything else is hashed rather than mangled, so two ids can never
// collide on one name. The true id always travels in MachineIDAnnotation.
func resourceName(machineID string) string {
	id := strings.ToLower(machineID)
	if len(id) <= 61 && dns1123Label.MatchString(id) {
		return "m-" + id
	}
	sum := sha256.Sum256([]byte(machineID))
	return "m-" + hex.EncodeToString(sum[:16])
}

func pvcName(machineID string) string    { return resourceName(machineID) + "-persist" }
func netpolName(machineID string) string { return resourceName(machineID) }

// handle is the driver-level handle the control plane persists as vm_handle. It
// is never parsed back; it exists so an operator reading the machines table can
// find the pod.
func (d *Driver) handle(machineID string) string {
	return "k8s:" + d.cfg.Namespace + "/" + resourceName(machineID)
}

// selector matches every object this driver owns.
func ownedSelector() string {
	return ManagedByLabel + "=" + managedByValue + "," + ComponentLabel + "=" + componentValue
}

// EnsureRunning brings a machine's PVC, NetworkPolicy and Pod to the desired
// state. It is idempotent: for a machine that is already running it changes
// nothing and returns the existing handle, which is what the control-plane
// poller leans on.
//
// A pod found in a terminal phase (Failed/Succeeded) is deleted and recreated —
// that is this backend's "reboot" path, and the only way EnsureRunning brings a
// crashed machine back.
func (d *Driver) EnsureRunning(ctx context.Context, spec driver.VMSpec) (string, error) {
	if len(spec.VolumeKey) > 0 {
		// Not an error: the control plane mints a key for every machine and has
		// no way to know this host cannot use it. Log once per ensure so the
		// regression is visible in the agent's logs, never the key itself.
		slog.Warn("kubernetes: volume key ignored — this backend has no host-side LUKS; "+
			"encryption at rest is the StorageClass's responsibility (see SECURITY.md)",
			"machine", spec.MachineID)
	}
	image := spec.ImageRef
	if image == "" {
		// Tracer-bullet fallback: before K3 lands image_ref, a kind-based e2e
		// puts the image reference in rootfs_ref. A configured DefaultImage wins
		// over neither — it is the last resort.
		image = spec.RootfsRef
	}
	if image == "" {
		image = d.cfg.DefaultImage
	}
	if image == "" {
		return "", fmt.Errorf("%w: machine %s has no image_ref (and no PROTEOS_K8S_DEFAULT_IMAGE) — "+
			"a kubernetes host cannot boot a template that only carries kernel_ref/rootfs_ref",
			driver.ErrBadSpec, spec.MachineID)
	}

	if err := d.ensurePVC(ctx, spec); err != nil {
		return "", err
	}
	if err := d.ensureNetworkPolicy(ctx, spec); err != nil {
		return "", err
	}
	if err := d.ensurePod(ctx, spec, image); err != nil {
		return "", err
	}
	return d.handle(spec.MachineID), nil
}

// ensurePod creates the machine pod if absent. An existing pod in a terminal
// phase is deleted first (foreground delete, so the recreate cannot race a
// half-gone object); an existing live pod is left exactly as it is — mutating a
// running pod's spec is not possible in Kubernetes anyway, and a spec change
// takes effect on the next stop/start, matching the firecracker driver's
// "resource overrides apply on the next boot".
func (d *Driver) ensurePod(ctx context.Context, spec driver.VMSpec, image string) error {
	pods := d.client.CoreV1().Pods(d.cfg.Namespace)
	name := resourceName(spec.MachineID)

	existing, err := pods.Get(ctx, name, metav1.GetOptions{})
	switch {
	case err == nil && !terminal(existing.Status.Phase):
		return nil
	case err == nil:
		slog.Info("kubernetes: recreating pod in a terminal phase",
			"machine", spec.MachineID, "pod", name, "phase", existing.Status.Phase)
		if err := d.deletePod(ctx, name); err != nil {
			return err
		}
	case !apierrors.IsNotFound(err):
		return fmt.Errorf("get pod %s: %w", name, err)
	}

	pod := d.buildPod(spec, image)
	if _, err := pods.Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Another EnsureRunning won the race; idempotent either way.
			return nil
		}
		return fmt.Errorf("create pod %s: %w", name, err)
	}
	slog.Info("kubernetes: machine pod created", "machine", spec.MachineID, "pod", name,
		"image", image, "runtime_class", d.cfg.RuntimeClass)
	return nil
}

// terminal reports whether a pod phase is one the kubelet will never leave, so
// the pod has to be replaced rather than waited on.
func terminal(p corev1.PodPhase) bool {
	return p == corev1.PodFailed || p == corev1.PodSucceeded
}

// deletePod removes a pod and waits for nothing — callers that need the name
// free again (the recreate path) delete with a zero grace period so the API
// server does not hand back a terminating object on the next Create.
func (d *Driver) deletePod(ctx context.Context, name string) error {
	zero := int64(0)
	err := d.client.CoreV1().Pods(d.cfg.Namespace).Delete(ctx, name, metav1.DeleteOptions{
		GracePeriodSeconds: &zero,
	})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete pod %s: %w", name, err)
	}
	return nil
}

// Stop deletes the machine pod and keeps its PVC, so a later EnsureRunning comes
// back to the same $HOME and workspace. Both stop modes do the same thing: a pod
// has no RAM to snapshot. A hibernate request is honoured as a poweroff and
// logged, so an operator looking at a mixed fleet can see why the machine came
// back cold.
func (d *Driver) Stop(ctx context.Context, machineID string, mode driver.StopMode) error {
	name := resourceName(machineID)
	if _, err := d.client.CoreV1().Pods(d.cfg.Namespace).Get(ctx, name, metav1.GetOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			// No pod, but a PVC means the machine exists and is already stopped.
			if d.pvcExists(ctx, machineID) {
				return nil
			}
			return driver.ErrUnknownMachine
		}
		return fmt.Errorf("get pod %s: %w", name, err)
	}

	if mode == driver.StopModeHibernate {
		slog.Info("kubernetes: hibernate downgraded to poweroff — pods have no RAM snapshot; "+
			"the volume is kept and the machine will boot cold",
			"machine", machineID)
	}

	grace := stopGracePeriod
	err := d.client.CoreV1().Pods(d.cfg.Namespace).Delete(ctx, name, metav1.DeleteOptions{
		GracePeriodSeconds: &grace,
	})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete pod %s: %w", name, err)
	}
	return nil
}

// Destroy removes every trace of a machine: pod, NetworkPolicy and PVC. It is
// idempotent — a NotFound on any of the three is success, so a retried destroy
// (or one racing a partially-completed earlier attempt) still converges.
func (d *Driver) Destroy(ctx context.Context, machineID string) error {
	name := resourceName(machineID)
	grace := stopGracePeriod

	err := d.client.CoreV1().Pods(d.cfg.Namespace).Delete(ctx, name, metav1.DeleteOptions{
		GracePeriodSeconds: &grace,
	})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete pod %s: %w", name, err)
	}

	err = d.client.NetworkingV1().NetworkPolicies(d.cfg.Namespace).
		Delete(ctx, netpolName(machineID), metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete networkpolicy %s: %w", netpolName(machineID), err)
	}

	err = d.client.CoreV1().PersistentVolumeClaims(d.cfg.Namespace).
		Delete(ctx, pvcName(machineID), metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete pvc %s: %w", pvcName(machineID), err)
	}
	return nil
}

// Reattach is a no-op beyond a sanity log. The firecracker and dev drivers have
// to reconcile on-disk records against live processes at startup; here the API
// server never stopped being the record, and machine pods kept running while
// the cluster-agent was down.
func (d *Driver) Reattach(ctx context.Context) error {
	pods, err := d.client.CoreV1().Pods(d.cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: ownedSelector(),
	})
	if err != nil {
		return fmt.Errorf("list machine pods: %w", err)
	}
	slog.Info("kubernetes: reattached", "namespace", d.cfg.Namespace, "pods", len(pods.Items))
	return nil
}

// pvcExists reports whether a machine's volume is still present — the signal
// that a machine exists but is stopped, as opposed to never having existed.
//
// A volume being deleted does NOT count. Kubernetes holds a pvc-protection
// finalizer on a claim while a pod still mounts it, so for the whole of a
// destroy the PVC remains visible to a Get; treating that as "the machine
// exists" makes a destroyed machine report stopped instead of gone.
func (d *Driver) pvcExists(ctx context.Context, machineID string) bool {
	pvc, err := d.client.CoreV1().PersistentVolumeClaims(d.cfg.Namespace).
		Get(ctx, pvcName(machineID), metav1.GetOptions{})
	return err == nil && !terminating(pvc.ObjectMeta)
}
