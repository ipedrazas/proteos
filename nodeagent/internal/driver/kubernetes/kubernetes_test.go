package kubernetes_test

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	api "github.com/tavon-ai/proteos/nodeagent/api"
	"github.com/tavon-ai/proteos/nodeagent/internal/driver"
	"github.com/tavon-ai/proteos/nodeagent/internal/driver/conformance"
	k8s "github.com/tavon-ai/proteos/nodeagent/internal/driver/kubernetes"
	"github.com/tavon-ai/proteos/nodeagent/internal/driver/kubernetes/fakecluster"
)

const (
	testNamespace = "proteos-machines"
	testImage     = "ghcr.io/tavon-ai/proteos-guest:base-test"
	failImage     = "ghcr.io/tavon-ai/proteos-guest:fail-boot"
)

// newDriver builds a driver over a simulated cluster whose pods become ready
// after bootDelay, so the async creating→running path is exercised rather than
// short-circuited.
func newDriver(t *testing.T, bootDelay time.Duration) (*k8s.Driver, *fakecluster.Cluster) {
	t.Helper()
	cluster := fakecluster.New(fakecluster.Options{
		Namespace:     testNamespace,
		BootDelay:     bootDelay,
		FailBootImage: failImage,
		Nodes: []fakecluster.Node{
			{Name: "node-a", Vcpus: 8, MemMiB: 16384},
			{Name: "node-b", Vcpus: 8, MemMiB: 16384},
			{Name: "node-cordoned", Vcpus: 64, MemMiB: 262144, Cordon: true},
			{Name: "node-down", Vcpus: 64, MemMiB: 262144, NotReady: true},
		},
	})
	d := k8s.New(cluster, k8s.Config{
		Namespace:      testNamespace,
		AgentNamespace: "proteos-system",
		AgentPodLabels: map[string]string{"app.kubernetes.io/name": "proteos-cluster-agent"},
		StorageClass:   "fast-encrypted",
		RuntimeClass:   "gvisor",
		ClusterCIDRs:   []string{"10.244.0.0/16"},
		CapacityTTL:    time.Nanosecond, // no caching in tests
	})
	return d, cluster
}

func spec(id string) driver.VMSpec {
	return driver.VMSpec{
		MachineID: id,
		Vcpus:     2,
		MemMiB:    2048,
		ImageRef:  testImage,
		Disks:     []driver.Disk{{ID: "disk-" + id, SizeMiB: 20480}},
	}
}

// TestConformance runs the shared driver-contract suite. It is the point of the
// whole exercise: the kubernetes backend has to behave like the others where the
// control plane cannot tell them apart.
func TestConformance(t *testing.T) {
	conformance.Run(t, conformance.Suite{
		NewDriver: func(t *testing.T) driver.Driver {
			d, _ := newDriver(t, 20*time.Millisecond)
			return d
		},
		Spec:                      spec,
		SupportsHibernateSnapshot: false, // pods have no RAM restore
	})
}

// --- pod shaping ------------------------------------------------------------

func TestEnsureBuildsTheExpectedPod(t *testing.T) {
	d, cluster := newDriver(t, 0)
	ctx := context.Background()
	const id = "aaaaaaaa-0000-4000-8000-000000000001"

	handle, err := d.EnsureRunning(ctx, spec(id))
	if err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	if want := "k8s:" + testNamespace + "/m-" + id; handle != want {
		t.Errorf("handle = %q, want %q", handle, want)
	}

	pod, err := cluster.CoreV1().Pods(testNamespace).Get(ctx, "m-"+id, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}

	if pod.Annotations["proteos.io/machine-id"] != id {
		t.Errorf("machine-id annotation = %q, want %q", pod.Annotations["proteos.io/machine-id"], id)
	}
	if pod.Spec.RuntimeClassName == nil || *pod.Spec.RuntimeClassName != "gvisor" {
		t.Error("machine pod is missing the sandboxed RuntimeClass — that is the whole isolation story on this backend")
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Error("machine pod mounts a service-account token; a machine is not a cluster client")
	}

	c := pod.Spec.Containers[0]
	if c.Image != testImage {
		t.Errorf("image = %q, want %q", c.Image, testImage)
	}
	if got := c.Resources.Requests.Cpu().MilliValue(); got != 2000 {
		t.Errorf("cpu request = %vm, want 2000m (ratio defaults to 1.0)", got)
	}
	if got := c.Resources.Limits.Cpu().Value(); got != 2 {
		t.Errorf("cpu limit = %v, want 2", got)
	}
	if got := c.Resources.Limits.Memory().Value(); got != 2048*1024*1024 {
		t.Errorf("memory limit = %v, want 2 GiB", got)
	}
	if c.SecurityContext.AllowPrivilegeEscalation == nil || *c.SecurityContext.AllowPrivilegeEscalation {
		t.Error("allowPrivilegeEscalation is not disabled")
	}
	if len(c.SecurityContext.Capabilities.Drop) != 1 || c.SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Errorf("capabilities.drop = %v, want [ALL]", c.SecurityContext.Capabilities.Drop)
	}

	// The env the guest-agent needs to listen on TCP and persist in dir mode.
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	for k, want := range map[string]string{
		"PROTEOS_GUEST_LISTEN":         "tcp:0.0.0.0:1024",
		"PROTEOS_GUEST_WEB_LISTEN":     "tcp:0.0.0.0:1025",
		"PROTEOS_GUEST_PREVIEW_LISTEN": "tcp:0.0.0.0:1026",
		"PROTEOS_GUEST_PERSIST":        "/persist",
		"PROTEOS_GUEST_ENV_DIR":        "/run/proteos/env",
	} {
		if env[k] != want {
			t.Errorf("env %s = %q, want %q", k, env[k], want)
		}
	}

	// /run/proteos must be memory-backed: the secrets package requires its env
	// dir to be tmpfs so injected credentials never touch a disk.
	var runVol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == "run-proteos" {
			runVol = &pod.Spec.Volumes[i]
		}
	}
	if runVol == nil || runVol.EmptyDir == nil || runVol.EmptyDir.Medium != corev1.StorageMediumMemory {
		t.Error("/run/proteos is not a memory-backed emptyDir; injected secrets would land on disk")
	}
}

// The readiness probe must not be an httpGet. The kubelet performs those from
// the node's address, so the machine's NetworkPolicy — whose whole job is to
// permit exactly one ingress peer on the guest ports — would have to permit a
// second one it cannot portably name. Worse, CNIs disagree about whether probe
// traffic is subject to policy at all, which would make readiness a property of
// the CNI rather than of the guest. An exec probe goes through the CRI instead.
func TestReadinessProbeDoesNotCrossTheNetwork(t *testing.T) {
	d, cluster := newDriver(t, 0)
	ctx := context.Background()
	const id = "aaaaaaaa-0000-4000-8000-00000000000a"

	if _, err := d.EnsureRunning(ctx, spec(id)); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	pod, err := cluster.CoreV1().Pods(testNamespace).Get(ctx, "m-"+id, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}

	probe := pod.Spec.Containers[0].ReadinessProbe
	if probe == nil {
		t.Fatal("no readiness probe: the driver would call a machine running before its guest-agent answers")
	}
	if probe.HTTPGet != nil {
		t.Error("readiness is an httpGet probe: that needs the kubelet to reach a guest port, " +
			"which the machine's NetworkPolicy restricts to the cluster-agent alone")
	}
	if probe.Exec == nil || len(probe.Exec.Command) == 0 {
		t.Fatalf("want an exec probe, got %+v", probe.ProbeHandler)
	}
	joined := strings.Join(probe.Exec.Command, " ")
	if !strings.Contains(joined, "127.0.0.1:1024/healthz") {
		t.Errorf("exec probe does not check the guest-agent's own /healthz over loopback: %q", joined)
	}
}

// CPU is compressible and memory is not, so the request ratio must scale only
// the former. A machine that is overcommitted on memory is not slow, it is
// OOM-killed — which would lose someone's agent session to a noisy neighbour.
func TestCPURequestRatioScalesCPUButNeverMemory(t *testing.T) {
	cluster := fakecluster.New(fakecluster.Options{Namespace: testNamespace})
	d := k8s.New(cluster, k8s.Config{Namespace: testNamespace, CPURequestRatio: 0.25})
	ctx := context.Background()
	const id = "aaaaaaaa-0000-4000-8000-00000000000b"

	if _, err := d.EnsureRunning(ctx, spec(id)); err != nil { // spec is 2 vcpu / 2048 MiB
		t.Fatalf("EnsureRunning: %v", err)
	}
	pod, err := cluster.CoreV1().Pods(testNamespace).Get(ctx, "m-"+id, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	res := pod.Spec.Containers[0].Resources

	if got := res.Requests.Cpu().MilliValue(); got != 500 {
		t.Errorf("cpu request = %vm, want 500m (2 vcpu × 0.25)", got)
	}
	if got := res.Limits.Cpu().Value(); got != 2 {
		t.Errorf("cpu limit = %v, want the full 2 vcpu — the ratio scales the request only", got)
	}
	if req, lim := res.Requests.Memory().Value(), res.Limits.Memory().Value(); req != lim {
		t.Errorf("memory request %v != limit %v: memory must never be overcommitted", req, lim)
	}
}

// An out-of-range ratio must fall back to the safe default rather than produce a
// pod the API server rejects (a request above its limit) or one with no request.
func TestCPURequestRatioOutOfRangeFallsBackToOne(t *testing.T) {
	for _, ratio := range []float64{0, -1, 2} {
		cluster := fakecluster.New(fakecluster.Options{Namespace: testNamespace})
		d := k8s.New(cluster, k8s.Config{Namespace: testNamespace, CPURequestRatio: ratio})
		const id = "aaaaaaaa-0000-4000-8000-00000000000c"

		if _, err := d.EnsureRunning(context.Background(), spec(id)); err != nil {
			t.Fatalf("ratio %v: EnsureRunning: %v", ratio, err)
		}
		pod, err := cluster.CoreV1().Pods(testNamespace).Get(context.Background(), "m-"+id, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("ratio %v: get pod: %v", ratio, err)
		}
		if got := pod.Spec.Containers[0].Resources.Requests.Cpu().MilliValue(); got != 2000 {
			t.Errorf("ratio %v: cpu request = %vm, want the 2000m default", ratio, got)
		}
	}
}

func TestEnsureCreatesThePVC(t *testing.T) {
	d, cluster := newDriver(t, 0)
	ctx := context.Background()
	const id = "aaaaaaaa-0000-4000-8000-000000000002"

	if _, err := d.EnsureRunning(ctx, spec(id)); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	pvc, err := cluster.CoreV1().PersistentVolumeClaims(testNamespace).
		Get(ctx, "m-"+id+"-persist", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pvc: %v", err)
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "fast-encrypted" {
		t.Error("pvc did not get the configured storage class")
	}
	if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.Value() != 20480*1024*1024 {
		t.Errorf("pvc size = %v, want 20 GiB", got.Value())
	}
	if pvc.Annotations["proteos.io/disk-id"] != "disk-"+id {
		t.Errorf("disk-id annotation = %q", pvc.Annotations["proteos.io/disk-id"])
	}
}

func TestEnsureWithoutAnImageIsAnError(t *testing.T) {
	d, _ := newDriver(t, 0)
	s := spec("aaaaaaaa-0000-4000-8000-000000000003")
	s.ImageRef = ""
	if _, err := d.EnsureRunning(context.Background(), s); err == nil {
		t.Fatal("expected an error for a machine with no image_ref")
	}
}

// A firecracker-shaped template reaching a kubernetes host during the tracer
// bullet still boots: rootfs_ref stands in for image_ref until K3 lands.
func TestRootfsRefIsAcceptedAsAnImageFallback(t *testing.T) {
	d, cluster := newDriver(t, 0)
	ctx := context.Background()
	const id = "aaaaaaaa-0000-4000-8000-000000000004"

	s := spec(id)
	s.ImageRef = ""
	s.RootfsRef = testImage
	if _, err := d.EnsureRunning(ctx, s); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	pod, err := cluster.CoreV1().Pods(testNamespace).Get(ctx, "m-"+id, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if pod.Spec.Containers[0].Image != testImage {
		t.Errorf("image = %q, want the rootfs_ref fallback %q", pod.Spec.Containers[0].Image, testImage)
	}
}

// --- state mapping ----------------------------------------------------------

func TestPendingPodIsCreating(t *testing.T) {
	d, _ := newDriver(t, time.Hour) // never becomes ready
	ctx := context.Background()
	const id = "bbbbbbbb-0000-4000-8000-000000000001"

	if _, err := d.EnsureRunning(ctx, spec(id)); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	st, err := d.Status(ctx, id)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.State != api.StateCreating {
		t.Errorf("state = %q, want creating for a Pending pod", st.State)
	}
}

func TestFailedPodIsAnError(t *testing.T) {
	d, _ := newDriver(t, 0)
	ctx := context.Background()
	const id = "bbbbbbbb-0000-4000-8000-000000000002"

	s := spec(id)
	s.ImageRef = failImage
	if _, err := d.EnsureRunning(ctx, s); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	st, err := d.Status(ctx, id)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.State != api.StateError {
		t.Fatalf("state = %q, want error", st.State)
	}
	if !strings.Contains(st.Reason, "Error") {
		t.Errorf("reason = %q, want the container's termination reason", st.Reason)
	}
}

// A crash-looping guest keeps the pod in Running (RestartPolicy: Always), so
// without the explicit CrashLoopBackOff check a broken machine would report
// "creating" forever and never surface to the user.
func TestCrashLoopIsAnError(t *testing.T) {
	d, cluster := newDriver(t, 0)
	ctx := context.Background()
	const id = "bbbbbbbb-0000-4000-8000-000000000003"

	if _, err := d.EnsureRunning(ctx, spec(id)); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	if err := cluster.CrashLoop(testNamespace, "m-"+id); err != nil {
		t.Fatalf("CrashLoop: %v", err)
	}
	st, err := d.Status(ctx, id)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.State != api.StateError {
		t.Errorf("state = %q, want error for a crash-looping guest", st.State)
	}
	if !strings.Contains(st.Reason, "CrashLoopBackOff") {
		t.Errorf("reason = %q, want CrashLoopBackOff", st.Reason)
	}
}

// Stop keeps the volume, so a stopped machine still exists (and is reported as
// stopped, not unknown) with its disk id intact.
func TestStopKeepsThePVCAndReportsStopped(t *testing.T) {
	d, cluster := newDriver(t, 0)
	ctx := context.Background()
	const id = "cccccccc-0000-4000-8000-000000000001"

	if _, err := d.EnsureRunning(ctx, spec(id)); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	if err := d.Stop(ctx, id, driver.StopModeHibernate); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	st, err := d.Status(ctx, id)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.State != api.StateStopped {
		t.Errorf("state = %q, want stopped", st.State)
	}
	if st.Snapshot.Present {
		t.Error("a hibernate on this backend must not claim a snapshot")
	}
	if st.Boot != api.BootCold {
		t.Errorf("boot = %q, want cold", st.Boot)
	}
	if st.DiskID != "disk-"+id {
		t.Errorf("disk id = %q, want it preserved from the PVC", st.DiskID)
	}
	if _, err := cluster.CoreV1().PersistentVolumeClaims(testNamespace).
		Get(ctx, "m-"+id+"-persist", metav1.GetOptions{}); err != nil {
		t.Errorf("stop deleted the machine's volume: %v", err)
	}
}

func TestDestroyRemovesPodPVCAndPolicy(t *testing.T) {
	d, cluster := newDriver(t, 0)
	ctx := context.Background()
	const id = "cccccccc-0000-4000-8000-000000000002"

	if _, err := d.EnsureRunning(ctx, spec(id)); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	if err := d.Destroy(ctx, id); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if _, err := cluster.CoreV1().Pods(testNamespace).Get(ctx, "m-"+id, metav1.GetOptions{}); err == nil {
		t.Error("pod survived Destroy")
	}
	if _, err := cluster.CoreV1().PersistentVolumeClaims(testNamespace).
		Get(ctx, "m-"+id+"-persist", metav1.GetOptions{}); err == nil {
		t.Error("pvc survived Destroy")
	}
	if _, err := cluster.NetworkingV1().NetworkPolicies(testNamespace).
		Get(ctx, "m-"+id, metav1.GetOptions{}); err == nil {
		t.Error("networkpolicy survived Destroy")
	}
}

// A pod in a terminal phase is the "reboot" path: EnsureRunning replaces it
// rather than waiting forever on something the kubelet will never revive.
func TestEnsureRecreatesATerminalPod(t *testing.T) {
	d, cluster := newDriver(t, 0)
	ctx := context.Background()
	const id = "cccccccc-0000-4000-8000-000000000003"

	failing := spec(id)
	failing.ImageRef = failImage
	if _, err := d.EnsureRunning(ctx, failing); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	if st, _ := d.Status(ctx, id); st.State != api.StateError {
		t.Fatalf("precondition: state = %q, want error", st.State)
	}

	if _, err := d.EnsureRunning(ctx, spec(id)); err != nil {
		t.Fatalf("EnsureRunning (recreate): %v", err)
	}
	st, err := d.Status(ctx, id)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.State != api.StateRunning {
		t.Errorf("state = %q, want running after the recreate", st.State)
	}
	pod, err := cluster.CoreV1().Pods(testNamespace).Get(ctx, "m-"+id, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if pod.Spec.Containers[0].Image != testImage {
		t.Error("the recreated pod kept the old image")
	}
}

// --- capacity ---------------------------------------------------------------

func TestCapacityCountsSchedulableNodesOnly(t *testing.T) {
	d, _ := newDriver(t, 0)
	ctx := context.Background()

	if _, err := d.EnsureRunning(ctx, spec("dddddddd-0000-4000-8000-000000000001")); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}

	cap, err := d.Capacity(ctx)
	if err != nil {
		t.Fatalf("Capacity: %v", err)
	}
	// node-a + node-b only: the cordoned and NotReady nodes must not inflate
	// the cluster's advertised capacity.
	if cap.TotalVcpus != 16 {
		t.Errorf("total vcpus = %d, want 16 (cordoned/NotReady nodes excluded)", cap.TotalVcpus)
	}
	if cap.TotalMemMiB != 32768 {
		t.Errorf("total mem = %d MiB, want 32768", cap.TotalMemMiB)
	}
	if cap.UsedVcpus != 2 || cap.UsedMemMiB != 2048 {
		t.Errorf("used = %d vcpus / %d MiB, want 2 / 2048", cap.UsedVcpus, cap.UsedMemMiB)
	}
	if cap.UsedDiskMiB != 20480 {
		t.Errorf("used disk = %d MiB, want 20480 (the machine's PVC)", cap.UsedDiskMiB)
	}
}

// --- reattach ---------------------------------------------------------------

// The API server is this driver's state, so a cluster-agent restart must find
// the machines that kept running without it.
func TestReattachFindsExistingMachines(t *testing.T) {
	d, cluster := newDriver(t, 0)
	ctx := context.Background()
	const id = "eeeeeeee-0000-4000-8000-000000000001"

	if _, err := d.EnsureRunning(ctx, spec(id)); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}

	// A second driver over the same cluster stands in for a restarted agent: it
	// has no local state at all.
	restarted := k8s.New(cluster, k8s.Config{Namespace: testNamespace})
	if err := restarted.Reattach(ctx); err != nil {
		t.Fatalf("Reattach: %v", err)
	}
	st, err := restarted.Status(ctx, id)
	if err != nil {
		t.Fatalf("Status after restart: %v", err)
	}
	if st.State != api.StateRunning {
		t.Errorf("state after restart = %q, want running", st.State)
	}
	if st.GuestIP == "" {
		t.Error("restarted agent lost the pod IP")
	}
}
