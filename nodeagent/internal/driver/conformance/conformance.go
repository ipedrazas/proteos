// Package conformance is the shared driver-contract suite. Every backend —
// dev, firecracker, kubernetes — implements the same driver.Driver interface,
// and the HTTP layer, the control-plane poller and the re-attach logic all lean
// on that contract behaving identically. Until now each driver tested its own
// version of "create it, watch it come up, stop it, destroy it", which is
// exactly the kind of duplication that lets a new backend quietly diverge.
//
// A driver package runs the suite from its own test:
//
//	func TestConformance(t *testing.T) {
//	    conformance.Run(t, conformance.Suite{
//	        NewDriver: func(t *testing.T) driver.Driver { ... },
//	        Spec:      func(id string) driver.VMSpec { ... },
//	    })
//	}
//
// The suite asserts only what the contract actually promises, so a backend is
// not forced into behaviour it cannot honour: whether a stop produces a snapshot
// is a capability flag, not an assumption.
package conformance

import (
	"context"
	"errors"
	"testing"
	"time"

	api "github.com/tavon-ai/proteos/nodeagent/api"
	"github.com/tavon-ai/proteos/nodeagent/internal/driver"
)

// Suite describes how to build and drive one backend.
type Suite struct {
	// NewDriver returns a driver with a per-test fixture (temp dirs, a fake
	// cluster, ...). It is called once per subtest, so subtests never share
	// state.
	NewDriver func(t *testing.T) driver.Driver

	// Spec builds a bootable machine spec for the backend under test: the dev
	// driver needs a kernel_ref, the kubernetes driver needs an image.
	Spec func(machineID string) driver.VMSpec

	// SupportsHibernateSnapshot is true for backends that can actually capture
	// guest RAM. Where it is false, the suite asserts the OPPOSITE — a stop
	// leaves no snapshot and the next boot is cold — because that is what the
	// control plane must be able to rely on for such a backend.
	SupportsHibernateSnapshot bool

	// Timeout bounds each wait-for-state. Default 5s.
	Timeout time.Duration
}

// Run executes the whole suite.
func Run(t *testing.T, s Suite) {
	t.Helper()
	if s.Timeout == 0 {
		s.Timeout = 5 * time.Second
	}
	t.Run("UnknownMachine", s.testUnknownMachine)
	t.Run("Lifecycle", s.testLifecycle)
	t.Run("EnsureIsIdempotent", s.testEnsureIdempotent)
	t.Run("StopKeepsTheMachine", s.testStopKeepsMachine)
	t.Run("DestroyIsIdempotent", s.testDestroyIdempotent)
	t.Run("ListReportsTrackedMachines", s.testList)
}

// testUnknownMachine: an id the driver has never seen must be distinguishable
// from one it is tracking, because the HTTP layer turns exactly this error into
// a 404 and the control plane's poller treats a 404 as "the machine is gone".
func (s Suite) testUnknownMachine(t *testing.T) {
	d := s.NewDriver(t)
	ctx := context.Background()

	if _, err := d.Status(ctx, "no-such-machine"); !errors.Is(err, driver.ErrUnknownMachine) {
		t.Errorf("Status(unknown) = %v, want ErrUnknownMachine", err)
	}
	if err := d.Stop(ctx, "no-such-machine", driver.StopModePoweroff); !errors.Is(err, driver.ErrUnknownMachine) {
		t.Errorf("Stop(unknown) = %v, want ErrUnknownMachine", err)
	}
}

// testLifecycle walks the whole contract: create → running → stop → stopped →
// destroy → unknown.
func (s Suite) testLifecycle(t *testing.T) {
	d := s.NewDriver(t)
	ctx := context.Background()
	const id = "11111111-1111-4111-8111-111111111111"

	handle, err := d.EnsureRunning(ctx, s.Spec(id))
	if err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	if handle == "" {
		t.Error("EnsureRunning returned an empty handle")
	}

	st := s.waitState(t, d, id, api.StateRunning)
	if st.Handle == "" {
		t.Error("running machine has an empty handle")
	}
	if !s.SupportsHibernateSnapshot && st.Boot != api.BootCold {
		t.Errorf("boot = %q, want cold on a backend with no RAM restore", st.Boot)
	}

	if err := d.Stop(ctx, id, driver.StopModeHibernate); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	st = s.waitState(t, d, id, api.StateStopped)
	if !s.SupportsHibernateSnapshot && st.Snapshot.Present {
		t.Error("backend reports a snapshot it cannot possibly have taken; " +
			"the control plane would try to resume from it")
	}

	// A stopped machine still exists and can be started again — that is the
	// difference between Stop and Destroy.
	if _, err := d.EnsureRunning(ctx, s.Spec(id)); err != nil {
		t.Fatalf("EnsureRunning after stop: %v", err)
	}
	s.waitState(t, d, id, api.StateRunning)

	if err := d.Destroy(ctx, id); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if _, err := d.Status(ctx, id); !errors.Is(err, driver.ErrUnknownMachine) {
		t.Errorf("Status after Destroy = %v, want ErrUnknownMachine", err)
	}
}

// testEnsureIdempotent: the control-plane poller calls EnsureRunning repeatedly
// for a machine that is already up. That must be a no-op returning the same
// handle, not a second boot.
func (s Suite) testEnsureIdempotent(t *testing.T) {
	d := s.NewDriver(t)
	ctx := context.Background()
	const id = "22222222-2222-4222-8222-222222222222"

	first, err := d.EnsureRunning(ctx, s.Spec(id))
	if err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	s.waitState(t, d, id, api.StateRunning)

	second, err := d.EnsureRunning(ctx, s.Spec(id))
	if err != nil {
		t.Fatalf("EnsureRunning (repeat): %v", err)
	}
	if first != second {
		t.Errorf("handle changed on a repeat ensure: %q → %q", first, second)
	}
	if st, err := d.Status(ctx, id); err != nil {
		t.Fatalf("Status: %v", err)
	} else if st.State != api.StateRunning {
		t.Errorf("repeat ensure disturbed a running machine: state = %q", st.State)
	}
}

// testStopKeepsMachine: a stopped machine is still a machine. Its disk survives
// and Status keeps answering, which is what lets a user start it again.
func (s Suite) testStopKeepsMachine(t *testing.T) {
	d := s.NewDriver(t)
	ctx := context.Background()
	const id = "33333333-3333-4333-8333-333333333333"

	if _, err := d.EnsureRunning(ctx, s.Spec(id)); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	s.waitState(t, d, id, api.StateRunning)

	if err := d.Stop(ctx, id, driver.StopModePoweroff); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	s.waitState(t, d, id, api.StateStopped)

	// Stopping twice is not an error: the control plane retries.
	if err := d.Stop(ctx, id, driver.StopModePoweroff); err != nil {
		t.Errorf("second Stop: %v", err)
	}
	if err := d.Destroy(ctx, id); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
}

// testDestroyIdempotent: Destroy is the cleanup the control plane retries until
// it succeeds, so a second call — and a call for a machine that never existed —
// must both be successes.
func (s Suite) testDestroyIdempotent(t *testing.T) {
	d := s.NewDriver(t)
	ctx := context.Background()
	const id = "44444444-4444-4444-8444-444444444444"

	if err := d.Destroy(ctx, "never-existed"); err != nil {
		t.Errorf("Destroy(unknown) = %v, want nil", err)
	}
	if _, err := d.EnsureRunning(ctx, s.Spec(id)); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	s.waitState(t, d, id, api.StateRunning)
	if err := d.Destroy(ctx, id); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if err := d.Destroy(ctx, id); err != nil {
		t.Errorf("second Destroy = %v, want nil", err)
	}
}

// testList: List is what the control plane reconciles against, so every machine
// the driver is tracking has to appear in it.
func (s Suite) testList(t *testing.T) {
	d := s.NewDriver(t)
	ctx := context.Background()
	ids := []string{
		"55555555-5555-4555-8555-555555555555",
		"66666666-6666-4666-8666-666666666666",
	}
	for _, id := range ids {
		if _, err := d.EnsureRunning(ctx, s.Spec(id)); err != nil {
			t.Fatalf("EnsureRunning %s: %v", id, err)
		}
		s.waitState(t, d, id, api.StateRunning)
	}

	list, err := d.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	seen := make(map[string]string, len(list))
	for _, st := range list {
		seen[st.MachineID] = st.State
	}
	for _, id := range ids {
		if state, ok := seen[id]; !ok {
			t.Errorf("List omitted machine %s (got %v)", id, seen)
		} else if state != api.StateRunning {
			t.Errorf("List reports %s as %q, want running", id, state)
		}
	}

	// A destroyed machine leaves the list.
	if err := d.Destroy(ctx, ids[0]); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	list, err = d.List(ctx)
	if err != nil {
		t.Fatalf("List after destroy: %v", err)
	}
	for _, st := range list {
		if st.MachineID == ids[0] {
			t.Errorf("List still reports destroyed machine %s as %q", ids[0], st.State)
		}
	}
}

// waitState polls Status until the machine reaches want, so the suite works for
// backends whose transitions are asynchronous (all of them).
func (s Suite) waitState(t *testing.T, d driver.Driver, id, want string) driver.Status {
	t.Helper()
	deadline := time.Now().Add(s.Timeout)
	var last driver.Status
	var lastErr error
	for time.Now().Before(deadline) {
		st, err := d.Status(context.Background(), id)
		if err == nil {
			last = st
			if st.State == want {
				return st
			}
		} else {
			lastErr = err
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("machine %s never reached %q (last state %q, reason %q, err %v)",
		id, want, last.State, last.Reason, lastErr)
	return last
}
