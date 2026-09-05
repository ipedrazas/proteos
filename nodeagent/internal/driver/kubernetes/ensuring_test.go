package kubernetes

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	api "github.com/tavon-ai/proteos/nodeagent/api"
	"github.com/tavon-ai/proteos/nodeagent/internal/driver"
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
