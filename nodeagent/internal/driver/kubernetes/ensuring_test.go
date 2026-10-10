package kubernetes

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	api "github.com/ipedrazas/proteos/nodeagent/api"
	"github.com/ipedrazas/proteos/nodeagent/internal/driver"
)

// An internal test, because the race it pins cannot be produced from outside:
// the control-plane poller polled a provisioning machine while its ensure was
// between creating the PVC and creating the pod, the driver answered "stopped"
// (a volume and no pod is what a stopped machine looks like), and the poller
// read that as a failed boot. Found by the control-plane kind e2e on its second
// machine; the first had been lucky with the poller's tick.
func TestStatusDuringAnInFlightEnsureIsCreatingNotStopped(t *testing.T) {
	const id = "e5000000-0000-4000-8000-000000000001"
	client := fake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "proteos-machines"}},
		// The state mid-ensure: a volume, no pod.
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
			Name: pvcName(id), Namespace: "proteos-machines",
		}},
	)
	d := New(client, Config{Namespace: "proteos-machines", DialTimeout: time.Millisecond})
	ctx := context.Background()

	// Without an ensure in flight this IS a stopped machine.
	st, err := d.Status(ctx, id)
	if err != nil || st.State != api.StateStopped {
		t.Fatalf("idle: state=%q err=%v, want stopped", st.State, err)
	}

	done := d.beginEnsure(id)
	st, err = d.Status(ctx, id)
	if err != nil || st.State != api.StateCreating {
		t.Fatalf("mid-ensure: state=%q err=%v, want creating", st.State, err)
	}

	// Counted: a second concurrent ensure finishing must not clear the first.
	done2 := d.beginEnsure(id)
	done2()
	if st, _ = d.Status(ctx, id); st.State != api.StateCreating {
		t.Fatalf("after one of two ensures finished: state=%q, want creating", st.State)
	}
	done()
	if st, _ = d.Status(ctx, id); st.State != api.StateStopped {
		t.Fatalf("after the ensure: state=%q, want stopped", st.State)
	}

	// And a machine that does not exist at all, mid-ensure, is creating rather
	// than unknown — the poller would otherwise start its retry path.
	const fresh = "e5000000-0000-4000-8000-000000000002"
	if _, err := d.Status(ctx, fresh); err != driver.ErrUnknownMachine {
		t.Fatalf("fresh idle: err=%v, want ErrUnknownMachine", err)
	}
	defer d.beginEnsure(fresh)()
	if st, err := d.Status(ctx, fresh); err != nil || st.State != api.StateCreating {
		t.Fatalf("fresh mid-ensure: state=%q err=%v, want creating", st.State, err)
	}
}

// The same race across two cluster-agent replicas (K6). The process-local map
// above answers only for the replica running the ensure; behind one Service the
// poller's Status can land on the other one, which has never heard of this
// machine's ensure. What it can see is the stamp the ensure left on the
// machine's NetworkPolicy — so a second driver over the SAME cluster must say
// "creating" while the first is mid-ensure, and "stopped" once it is done.
func TestStatusOnAnotherReplicaSeesTheInFlightEnsure(t *testing.T) {
	const id = "e5000000-0000-4000-8000-000000000003"
	client := fake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "proteos-machines"}},
		// As the driver would have written it, so List can find it by label.
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
			Name: pvcName(id), Namespace: "proteos-machines",
			Labels:      map[string]string{ManagedByLabel: managedByValue, ComponentLabel: componentValue},
			Annotations: map[string]string{MachineIDAnnotation: id},
		}},
	)
	cfg := Config{Namespace: "proteos-machines", DialTimeout: time.Millisecond}
	replicaA, replicaB := New(client, cfg), New(client, cfg)
	ctx := context.Background()
	spec := driver.VMSpec{MachineID: id, Vcpus: 1, MemMiB: 1024, ImageRef: "img"}

	// Idle: a volume and no pod is a stopped machine, from either replica.
	if st, err := replicaB.Status(ctx, id); err != nil || st.State != api.StateStopped {
		t.Fatalf("idle on B: state=%q err=%v, want stopped", st.State, err)
	}

	// Replica A begins an ensure: the policy is the first thing it writes, and
	// it carries the stamp. Nothing else about A's ensure is visible to B.
	stamp, err := replicaA.ensureNetworkPolicy(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if st, err := replicaB.Status(ctx, id); err != nil || st.State != api.StateCreating {
		t.Fatalf("mid-ensure on B: state=%q err=%v, want creating (the stamp on the policy)", st.State, err)
	}
	// And in List, where the control plane's reconciliation would see it.
	list, err := replicaB.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].State != api.StateCreating {
		t.Fatalf("List on B mid-ensure = %+v, want one creating machine", list)
	}

	// A concurrent, later ensure stamps again; A finishing must clear only ITS
	// stamp, so the later ensure stays visible.
	stamp2, err := replicaB.ensureNetworkPolicy(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := replicaA.clearEnsureMarker(ctx, id, stamp); err != nil {
		t.Fatal(err)
	}
	if st, _ := replicaA.Status(ctx, id); st.State != api.StateCreating {
		t.Fatalf("after A cleared its own stamp with B's ensure in flight: state=%q, want creating", st.State)
	}
	if err := replicaB.clearEnsureMarker(ctx, id, stamp2); err != nil {
		t.Fatal(err)
	}
	if st, err := replicaA.Status(ctx, id); err != nil || st.State != api.StateStopped {
		t.Fatalf("after both ensures: state=%q err=%v, want stopped", st.State, err)
	}
}

// A stamp nobody cleared — the agent died mid-ensure — must not make a stopped
// machine report creating forever, and a Stop must clear one regardless of who
// wrote it.
func TestStaleEnsureMarkerExpiresAndStopClearsIt(t *testing.T) {
	const id = "e5000000-0000-4000-8000-000000000004"
	client := fake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "proteos-machines"}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
			Name: pvcName(id), Namespace: "proteos-machines",
		}},
	)
	d := New(client, Config{Namespace: "proteos-machines", DialTimeout: time.Millisecond, DeleteWaitTimeout: time.Second})
	ctx := context.Background()
	spec := driver.VMSpec{MachineID: id, Vcpus: 1, MemMiB: 1024, ImageRef: "img"}

	if _, err := d.ensureNetworkPolicy(ctx, spec); err != nil {
		t.Fatal(err)
	}
	// Age the stamp past the TTL (2 × delete-wait + 30s with the config above).
	pol, err := client.NetworkingV1().NetworkPolicies("proteos-machines").Get(ctx, netpolName(id), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pol.Annotations[EnsuredAtAnnotation] = time.Now().Add(-d.ensureMarkerTTL() - time.Second).UTC().Format(time.RFC3339Nano)
	if _, err := client.NetworkingV1().NetworkPolicies("proteos-machines").Update(ctx, pol, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if st, _ := d.Status(ctx, id); st.State != api.StateStopped {
		t.Fatalf("with an expired stamp: state=%q, want stopped", st.State)
	}

	// A fresh stamp, then a Stop (no pod: the machine is already stopped, which
	// Stop accepts) — the stamp must be gone afterwards.
	if _, err := d.ensureNetworkPolicy(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if st, _ := d.Status(ctx, id); st.State != api.StateCreating {
		t.Fatalf("with a fresh stamp: state=%q, want creating", st.State)
	}
	if err := d.Stop(ctx, id, driver.StopModePoweroff); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if st, _ := d.Status(ctx, id); st.State != api.StateStopped {
		t.Fatalf("after Stop: state=%q, want stopped (the stamp should be cleared)", st.State)
	}

	// And an ensure that RAN TO COMPLETION leaves no stamp behind: the pod
	// speaks for itself from then on, and a pod that later dies must read as
	// stopping/stopped, not creating.
	if _, err := d.EnsureRunning(ctx, spec); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	pol, err = client.NetworkingV1().NetworkPolicies("proteos-machines").Get(ctx, netpolName(id), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, still := pol.Annotations[EnsuredAtAnnotation]; still {
		t.Fatal("the ensure stamp survived a completed ensure")
	}
}
