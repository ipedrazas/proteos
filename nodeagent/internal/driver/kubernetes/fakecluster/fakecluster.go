// Package fakecluster wraps client-go's fake clientset with just enough kubelet
// behaviour to drive the kubernetes driver in tests.
//
// The fake clientset is a plain object store: it accepts a Pod and stores it
// verbatim, so a pod created there stays in the empty phase forever. Nothing
// schedules it, nothing pulls an image, nothing runs a readiness probe. That
// makes every lifecycle assertion ("the machine reaches running") impossible
// out of the box.
//
// This package adds the missing half: a created machine pod goes Pending, then
// (after BootDelay) Running with a pod IP and a ready guest container — the same
// shape a real kubelet reports, and the same async creating→running path the dev
// driver's boot delay exercises. It is a NON-test package on purpose, so the
// node-agent's HTTP-layer tests can run the driver contract against the
// kubernetes driver the same way they do against dev.
package fakecluster

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// Options tunes the simulated cluster.
type Options struct {
	// Namespace the machine pods live in. Also created as a Namespace object so
	// the driver's preflight finds it.
	Namespace string

	// BootDelay is how long a pod stays Pending before it becomes Running and
	// ready. Zero means "ready on create", which keeps tests that only care
	// about the end state fast.
	BootDelay time.Duration

	// FailBootImage makes any pod created with this image go to Failed instead
	// of Running, so the error path can be exercised end to end.
	FailBootImage string

	// Nodes to report from the node lister, for Capacity. Each entry is
	// (cpu cores, memory MiB).
	Nodes []Node
}

// Node is one schedulable node in the simulated cluster.
type Node struct {
	Name     string
	Vcpus    int
	MemMiB   int
	Cordon   bool
	NotReady bool
}

// Cluster is a fake clientset plus the goroutines standing in for a kubelet.
type Cluster struct {
	kubernetes.Interface

	client *fake.Clientset
	opts   Options
	nextIP atomic.Int32

	wg sync.WaitGroup
}

var podsGVR = schema.GroupVersionResource{Version: "v1", Resource: "pods"}

// guestContainer must match the driver's container name; the driver reads
// readiness per container, not per pod.
const guestContainer = "guest"

// New builds the simulated cluster. Close it (or let the test end) when done.
func New(opts Options) *Cluster {
	if opts.Namespace == "" {
		opts.Namespace = "proteos-machines"
	}

	objects := []runtime.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: opts.Namespace}},
	}
	for _, n := range opts.Nodes {
		objects = append(objects, node(n))
	}

	client := fake.NewClientset(objects...)
	c := &Cluster{Interface: client, client: client, opts: opts}
	c.nextIP.Store(9)

	// The reactor mutates the object the caller is creating (the tracker stores
	// the same pointer) and returns "not handled", so the normal create still
	// happens. With no boot delay the final status is written inline; with one,
	// the pod starts Pending and a goroutine plays kubelet later.
	//
	// Nothing here may call back into the clientset: the reactor runs while the
	// Fake holds its own lock, so a Get from inside it deadlocks.
	client.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		create, ok := action.(k8stesting.CreateAction)
		if !ok {
			return false, nil, nil
		}
		pod, ok := create.GetObject().(*corev1.Pod)
		if !ok {
			return false, nil, nil
		}
		ip := fmt.Sprintf("10.42.0.%d", c.nextIP.Add(1))
		failed := opts.FailBootImage != "" && podImage(pod) == opts.FailBootImage

		if opts.BootDelay <= 0 {
			settle(pod, ip, failed)
			return false, nil, nil
		}
		pod.Status.Phase = corev1.PodPending
		c.scheduleLater(pod.Namespace, pod.Name, ip, failed)
		return false, nil, nil
	})
	return c
}

// Wait blocks until every simulated boot has settled, so a test can assert on a
// steady state without polling.
func (c *Cluster) Wait() { c.wg.Wait() }

// settle writes the status a kubelet would report once the pod has finished
// starting: Running with a pod IP and a ready guest container, or Failed with
// the container's termination reason.
func settle(pod *corev1.Pod, ip string, failed bool) {
	if failed {
		pod.Status.Phase = corev1.PodFailed
		pod.Status.PodIP = ""
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: guestContainer,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				Reason: "Error", ExitCode: 1,
			}},
		}}
		return
	}
	pod.Status.Phase = corev1.PodRunning
	pod.Status.PodIP = ip
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  guestContainer,
		Ready: true,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	}}
}

// scheduleLater flips a pod to its final phase after BootDelay, so the async
// creating→running path is exercised rather than short-circuited.
func (c *Cluster) scheduleLater(namespace, name, ip string, failed bool) {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		time.Sleep(c.opts.BootDelay)
		pod, err := c.client.CoreV1().Pods(namespace).Get(context.Background(), name, metav1.GetOptions{})
		if err != nil {
			return // deleted before it finished "booting"
		}
		settle(pod, ip, failed)
		_ = c.client.Tracker().Update(podsGVR, pod, namespace)
	}()
}

// CrashLoop puts a running machine pod into CrashLoopBackOff, so the driver's
// "a pod that is Running but broken is an error, not creating" mapping can be
// tested against a real object rather than a hand-built struct.
func (c *Cluster) CrashLoop(namespace, name string) error {
	pod, err := c.client.CoreV1().Pods(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	pod.Status.Phase = corev1.PodRunning
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  guestContainer,
		Ready: false,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
			Reason: "CrashLoopBackOff",
		}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Reason: "Error", ExitCode: 2,
		}},
	}}
	return c.client.Tracker().Update(podsGVR, pod, namespace)
}

func node(n Node) *corev1.Node {
	ready := corev1.ConditionTrue
	if n.NotReady {
		ready = corev1.ConditionFalse
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: n.Name},
		Spec:       corev1.NodeSpec{Unschedulable: n.Cordon},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    *quantity(int64(n.Vcpus)),
				corev1.ResourceMemory: *quantity(int64(n.MemMiB) * 1024 * 1024),
			},
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: ready}},
		},
	}
}

// quantity is a resource.Quantity for a plain integer count.
func quantity(v int64) *resource.Quantity { return resource.NewQuantity(v, resource.DecimalSI) }

func podImage(pod *corev1.Pod) string {
	if len(pod.Spec.Containers) == 0 {
		return ""
	}
	return pod.Spec.Containers[0].Image
}
