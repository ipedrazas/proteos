package machine_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ipedrazas/proteos/controlplane/internal/machine"
	"github.com/ipedrazas/proteos/controlplane/internal/secrets"
	"github.com/ipedrazas/proteos/controlplane/internal/store"
	"github.com/ipedrazas/proteos/controlplane/internal/testutil"
	agentapi "github.com/ipedrazas/proteos/nodeagent/api"
)

// K3: the control-plane half of the Kubernetes backend. A cluster-agent is an
// ordinary host row that differs in two ways the control plane must act on
// before it ever calls the agent — it boots an OCI image rather than a kernel +
// ext4 rootfs, and it cannot hibernate. These tests pin both.

// recordingAgent is a machine.NodeClient that records what the service asked
// for. It reports abundant capacity so the multi-host scheduler path is decided
// by template/driver compatibility alone, which is what these tests are about.
type recordingAgent struct {
	mu         sync.Mutex
	ensures    map[string]agentapi.EnsureRequest
	stopModes  []string
	stopCalls  int
	destroyIDs []string
}

func newRecordingAgent() *recordingAgent {
	return &recordingAgent{ensures: map[string]agentapi.EnsureRequest{}}
}

func (a *recordingAgent) Ensure(_ context.Context, id string, req agentapi.EnsureRequest) (agentapi.EnsureResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ensures[id] = req
	return agentapi.EnsureResponse{Handle: "h-" + id[:8]}, nil
}

func (a *recordingAgent) Stop(_ context.Context, _, mode string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stopCalls++
	a.stopModes = append(a.stopModes, mode)
	return nil
}

func (a *recordingAgent) Status(_ context.Context, id string) (agentapi.MachineStatus, error) {
	return agentapi.MachineStatus{MachineID: id, State: agentapi.StateCreating}, nil
}

func (a *recordingAgent) List(context.Context) ([]agentapi.MachineStatus, error) { return nil, nil }

func (a *recordingAgent) Destroy(_ context.Context, id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.destroyIDs = append(a.destroyIDs, id)
	return nil
}

func (a *recordingAgent) Capacity(context.Context, pgtype.UUID) (agentapi.CapacityResponse, error) {
	return agentapi.CapacityResponse{TotalVcpus: 64, TotalMemMiB: 262144}, nil
}

func (a *recordingAgent) ensureFor(t *testing.T, id string) agentapi.EnsureRequest {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	req, ok := a.ensures[id]
	if !ok {
		t.Fatalf("no ensure recorded for machine %s", id)
	}
	return req
}

// backendCatalog names one template per backend plus one that carries both
// images — the mixed fleet the scheduler has to sort out.
func backendCatalog(t *testing.T) machine.Catalog {
	t.Helper()
	defaults := machine.Resources{Vcpus: 2, MemMiB: 2048, DiskMiB: 10240}
	c, err := machine.NewCatalog([]machine.Template{
		{ID: "fc", Label: "Firecracker only", RootfsRef: "rootfs-base.ext4", Defaults: defaults},
		{ID: "k8s", Label: "Image only", ImageRef: "ghcr.io/ipedrazas/proteos-guest:base", Defaults: defaults},
		{ID: "both", Label: "Both", RootfsRef: "rootfs-base.ext4",
			ImageRef: "ghcr.io/ipedrazas/proteos-guest:base", Defaults: defaults},
	}, "vmlinux-6.1")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

type backendFixture struct {
	pool   *pgxpool.Pool
	q      *store.Queries
	svc    *machine.Service
	agent  *recordingAgent
	userID pgtype.UUID
	hosts  map[string]store.Host // host name -> row
}

// newBackendFixture seeds a user and one host per named driver kind
// (name -> driver), and wires a service over the backendCatalog.
func newBackendFixture(t *testing.T, hosts map[string]string) *backendFixture {
	t.Helper()
	pool, q := testutil.Postgres(t)
	ctx := context.Background()

	user, err := q.UpsertUser(ctx, store.UpsertUserParams{GithubUserID: 7, Login: "u"})
	if err != nil {
		t.Fatal(err)
	}
	seeded := make(map[string]store.Host, len(hosts))
	for name, driver := range hosts {
		h, err := q.UpsertHostByName(ctx, store.UpsertHostByNameParams{
			Name: name, AgentUrl: "http://" + name, Driver: driver,
		})
		if err != nil {
			t.Fatal(err)
		}
		seeded[name] = h
	}

	agent := newRecordingAgent()
	svc := machine.NewService(pool, agent, machine.NewBroker(), secrets.NewMemStore(), machine.Spec{
		Vcpus: 2, MemMiB: 2048, DiskMiB: 10240, KernelRef: "vmlinux-6.1", RootfsRef: "rootfs-base.ext4",
		Catalog: backendCatalog(t), Limits: machine.NewResourceLimits(8, 16384, 51200),
	})
	return &backendFixture{pool: pool, q: q, svc: svc, agent: agent, userID: user.ID, hosts: seeded}
}

// running creates a machine from the named template and forces it to running, so
// Stop can be exercised without driving the poller.
func (f *backendFixture) running(t *testing.T, templateID string) store.Machine {
	t.Helper()
	ctx := context.Background()
	m, err := f.svc.Create(ctx, f.userID, machine.CreateOptions{TemplateID: templateID})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := f.pool.Exec(ctx, "UPDATE machines SET state='running' WHERE id=$1", m.ID); err != nil {
		t.Fatalf("force running: %v", err)
	}
	m.State = string(machine.StateRunning)
	return m
}

// A machine on a cluster host is powered off, not hibernated: a pod has no RAM
// to snapshot, so recording `hibernating` would claim the user's working state
// was preserved when it was not.
func TestStopOnKubernetesHostPowersOff(t *testing.T) {
	t.Parallel()
	f := newBackendFixture(t, map[string]string{"k8s-1": agentapi.DriverKubernetes})
	m := f.running(t, "k8s")

	stopped, err := f.svc.Stop(context.Background(), f.userID, m.ID)
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if stopped.State != string(machine.StateStopping) {
		t.Fatalf("state = %q, want %q", stopped.State, machine.StateStopping)
	}
	if got := f.agent.stopModes; len(got) != 1 || got[0] != agentapi.StopModePoweroff {
		t.Fatalf("stop modes = %v, want [%s]", got, agentapi.StopModePoweroff)
	}
}

// The Firecracker path is unchanged: stop still means hibernate.
func TestStopOnFirecrackerHostHibernates(t *testing.T) {
	t.Parallel()
	f := newBackendFixture(t, map[string]string{"fc-1": agentapi.DriverFirecracker})
	m := f.running(t, "fc")

	stopped, err := f.svc.Stop(context.Background(), f.userID, m.ID)
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if stopped.State != string(machine.StateHibernating) {
		t.Fatalf("state = %q, want %q", stopped.State, machine.StateHibernating)
	}
	if got := f.agent.stopModes; len(got) != 1 || got[0] != agentapi.StopModeHibernate {
		t.Fatalf("stop modes = %v, want [%s]", got, agentapi.StopModeHibernate)
	}
}

// A machine created from an image template pins the image on its row and sends
// it on every ensure; a Firecracker one has no image at all (NULL, not "").
func TestCreatePinsImageRefAndSendsItOnEnsure(t *testing.T) {
	t.Parallel()
	f := newBackendFixture(t, map[string]string{
		"fc-1":  agentapi.DriverFirecracker,
		"k8s-1": agentapi.DriverKubernetes,
	})
	ctx := context.Background()

	k8sMachine, err := f.svc.Create(ctx, f.userID, machine.CreateOptions{TemplateID: "k8s"})
	if err != nil {
		t.Fatalf("create k8s machine: %v", err)
	}
	if k8sMachine.ImageRef == nil || *k8sMachine.ImageRef != "ghcr.io/ipedrazas/proteos-guest:base" {
		t.Fatalf("image_ref = %v, want the template's image", k8sMachine.ImageRef)
	}
	req := f.agent.ensureFor(t, machine.UUIDString(k8sMachine.ID))
	if req.ImageRef != "ghcr.io/ipedrazas/proteos-guest:base" {
		t.Fatalf("ensure image_ref = %q", req.ImageRef)
	}

	fcMachine, err := f.svc.Create(ctx, f.userID, machine.CreateOptions{TemplateID: "fc"})
	if err != nil {
		t.Fatalf("create firecracker machine: %v", err)
	}
	if fcMachine.ImageRef != nil {
		t.Fatalf("image_ref = %v, want NULL for a template with no image", *fcMachine.ImageRef)
	}
	if req := f.agent.ensureFor(t, machine.UUIDString(fcMachine.ID)); req.ImageRef != "" {
		t.Fatalf("ensure image_ref = %q, want empty (omitted)", req.ImageRef)
	}
}

// Placement is by backend compatibility: each template lands on the host that
// can actually boot it, whatever the free capacity says.
func TestSchedulerPlacesTemplatesOnCompatibleHosts(t *testing.T) {
	t.Parallel()
	f := newBackendFixture(t, map[string]string{
		"fc-1":  agentapi.DriverFirecracker,
		"k8s-1": agentapi.DriverKubernetes,
	})
	ctx := context.Background()

	for _, tc := range []struct{ template, host string }{
		{"fc", "fc-1"},
		{"k8s", "k8s-1"},
	} {
		m, err := f.svc.Create(ctx, f.userID, machine.CreateOptions{TemplateID: tc.template})
		if err != nil {
			t.Fatalf("create %q: %v", tc.template, err)
		}
		if want := f.hosts[tc.host].ID; m.HostID != want {
			t.Fatalf("template %q placed on the wrong host: %v, want %s", tc.template, m.HostID, tc.host)
		}
	}
}

// A template no active host can boot is refused at create, with a message that
// names the backends it needs. Without the filter the machine would be placed
// anyway and then fail at the agent with driver.ErrBadSpec, leaving the user
// with a broken machine and an error about refs they never chose.
func TestCreateRefusesTemplateNoHostCanBoot(t *testing.T) {
	t.Parallel()
	f := newBackendFixture(t, map[string]string{"fc-1": agentapi.DriverFirecracker})

	_, err := f.svc.Create(context.Background(), f.userID, machine.CreateOptions{TemplateID: "k8s"})
	if !errors.Is(err, machine.ErrNoCapacity) {
		t.Fatalf("err = %v, want ErrNoCapacity", err)
	}
	if !strings.Contains(err.Error(), agentapi.DriverKubernetes) {
		t.Fatalf("error should name the backend the template needs: %v", err)
	}
	if f.agent.stopCalls != 0 || len(f.agent.ensures) != 0 {
		t.Fatal("the agent should not have been called at all")
	}
}

// A template carrying both images is placeable on either backend — the mixed
// fleet's normal case, and the reason SupportsDriver is a per-template question
// rather than a global mode.
func TestTemplateWithBothImagesRunsOnEitherBackend(t *testing.T) {
	t.Parallel()
	for _, driver := range []string{agentapi.DriverFirecracker, agentapi.DriverKubernetes} {
		f := newBackendFixture(t, map[string]string{"only": driver})
		m, err := f.svc.Create(context.Background(), f.userID, machine.CreateOptions{TemplateID: "both"})
		if err != nil {
			t.Fatalf("create on %s host: %v", driver, err)
		}
		req := f.agent.ensureFor(t, machine.UUIDString(m.ID))
		if req.RootfsRef == "" || req.ImageRef == "" {
			t.Fatalf("both refs should reach the agent, which picks the one it can use: %+v", req)
		}
	}
}
