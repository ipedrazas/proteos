//go:build kind_e2e

// Behind the `kind_e2e` build tag: it needs a live cluster-agent backed by a
// real Kubernetes API server, which deploy/kubernetes/e2e-kind.sh provides. The
// driver-level suite in nodeagent/internal/driver/kubernetes drives that agent
// directly, as the control plane would; this file puts the CONTROL PLANE in
// front of it, so the K3 placement path (a host whose driver is kubernetes, a
// template that carries an image_ref, poweroff instead of hibernate) and the
// machine-web preview proxy with its two port allowlists are exercised against a
// real machine rather than against a stub node client.
package httpapi_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tavon-ai/proteos/controlplane/internal/auth"
	"github.com/tavon-ai/proteos/controlplane/internal/gateway"
	"github.com/tavon-ai/proteos/controlplane/internal/httpapi"
	"github.com/tavon-ai/proteos/controlplane/internal/machine"
	"github.com/tavon-ai/proteos/controlplane/internal/nodeclient"
	"github.com/tavon-ai/proteos/controlplane/internal/secrets"
	"github.com/tavon-ai/proteos/controlplane/internal/session"
	"github.com/tavon-ai/proteos/controlplane/internal/store"
	"github.com/tavon-ai/proteos/controlplane/internal/testutil"
	guestwire "github.com/tavon-ai/proteos/guestagent/api"
	agentapi "github.com/tavon-ai/proteos/nodeagent/api"
)

// Environment, supplied by e2e-kind.sh (the same variables the driver suite
// reads, plus the agent's certificate so the control plane can PIN it the way
// production does with PROTEOS_NODE_CA_FILE, rather than skipping verification).
//
//	PROTEOS_E2E_AGENT_URL    https://127.0.0.1:<port> (a kubectl port-forward)
//	PROTEOS_E2E_AGENT_TOKEN  the shared bearer token
//	PROTEOS_E2E_AGENT_CA     path to the agent's certificate
//	PROTEOS_E2E_GUEST_IMAGE  the guest image loaded into the kind node
const (
	kindBootTimeout = 4 * time.Minute
	kindStopTimeout = 2 * time.Minute

	// The kind overlay narrows the cluster-agent's preview allowlist to
	// [kindAgentPreviewMin, kindAgentPreviewMax] (deploy/kubernetes/overlays/kind
	// /agent-env.yaml); the control plane in this test keeps the defaults. A port
	// in the agent's range is served, one above it is admitted by the control
	// plane's mint and refused by the agent — which is the layering under test.
	kindAgentPreviewMax = 8080
	kindAgentPreviewOut = 9000
)

func kindNodes(t *testing.T) (*nodeclient.Client, string) {
	t.Helper()
	url, token, ca := os.Getenv("PROTEOS_E2E_AGENT_URL"), os.Getenv("PROTEOS_E2E_AGENT_TOKEN"), os.Getenv("PROTEOS_E2E_AGENT_CA")
	if url == "" || token == "" || ca == "" {
		t.Fatal("PROTEOS_E2E_AGENT_URL, _TOKEN and _CA must be set (run deploy/kubernetes/e2e-kind.sh)")
	}
	image := os.Getenv("PROTEOS_E2E_GUEST_IMAGE")
	if image == "" {
		image = "proteos-guest:e2e"
	}
	nodes, err := nodeclient.NewPinned(url, token, ca)
	if err != nil {
		t.Fatalf("pinned node client: %v", err)
	}
	return nodes, image
}

// setupCPKind wires a control plane whose only host is the cluster-agent and
// whose only template boots an OCI image — the production shape of a
// Kubernetes-only fleet — with a fast poller and the machine-web origin.
func setupCPKind(t *testing.T, nodes *nodeclient.Client, image string) (cpFixture, *machine.Service) {
	t.Helper()
	ctx := context.Background()
	pool, q := testutil.Postgres(t)

	user, err := q.UpsertUser(ctx, store.UpsertUserParams{GithubUserID: 5151, Login: "kind-e2e", Email: "k@x", AvatarUrl: ""})
	if err != nil {
		t.Fatal(err)
	}
	// The host row is what tells the scheduler this host boots images and
	// cannot hibernate (K3). Everything below follows from this one column.
	if _, err := q.UpsertHostByName(ctx, store.UpsertHostByNameParams{
		Name: "kind", AgentUrl: nodes.BaseURL(), Driver: agentapi.DriverKubernetes,
	}); err != nil {
		t.Fatal(err)
	}

	registry := gateway.NewRegistry()
	sessions := session.NewManager(q, time.Hour)
	sessions.SetRevocationListener(registry)
	token, err := sessions.Create(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}

	broker := machine.NewBroker()
	svc := machine.NewService(pool, nodes, broker, secrets.NewMemStore(), machine.Spec{
		Vcpus: 1, MemMiB: 1024, DiskMiB: 1024,
		// No kernel_ref/rootfs_ref: this template can ONLY be placed on a
		// kubernetes host. The scheduler filter is what makes that a placement
		// rather than a driver.ErrBadSpec later.
		ImageRef:         image,
		SessionExportDir: t.TempDir(),
	})
	poller := machine.NewPoller(pool, nodes, broker)
	pollCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go func() {
		tk := time.NewTicker(500 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-pollCtx.Done():
				return
			case <-tk.C:
				poller.AdvanceTransitional(pollCtx)
			}
		}
	}()

	origins := []string{testWSOrigin}
	gw := gateway.NewProxy(origins, nodes, registry)
	sessRes, machRes := httpapi.MachineWebResolvers(sessions, svc)
	machineWeb := gateway.NewMachineWeb(gateway.MachineWebConfig{
		Domain:         "localhost",
		SigningKey:     []byte("kind-e2e-machine-web-signing-key"),
		CookieSecure:   false,
		FrameAncestors: origins,
		Guests:         nodes,
		Registry:       registry,
		Sessions:       sessRes,
		Machines:       machRes,
		// Defaults: the control plane admits the whole high range. The AGENT's
		// range is the narrow one here (see kindAgentPreviewMax).
	})
	srv := &httpapi.Server{Sessions: sessions, Machines: svc, Broker: broker, Queries: q, Gateway: gw, MachineWeb: machineWeb}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return cpFixture{url: ts.URL, token: token, sessions: sessions, pool: pool, q: q, userID: user.ID}, svc
}

// waitCPStateFor is waitCPState with a timeout sized for a real pod: scheduling,
// a PVC bind and an image unpack stand between create and running.
func waitCPStateFor(t *testing.T, fx cpFixture, want machine.State, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		m, err := fx.q.GetMachineByUserID(context.Background(), fx.userID)
		if err == nil {
			last = m.State
			if m.State == string(want) {
				return
			}
			if m.State == string(machine.StateError) && want != machine.StateError {
				t.Fatalf("machine went to error while waiting for %q: %s", want, lastEventPayload(t, fx, m.ID))
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("machine never reached %q in %s (last=%q)", want, timeout, last)
}

// lastEventPayload is the most recent event's payload for a machine — where
// the poller records WHY it moved a machine to error. Without it a failed boot
// in this suite is a state name and nothing else.
func lastEventPayload(t *testing.T, fx cpFixture, id pgtype.UUID) string {
	t.Helper()
	evs, err := fx.q.ListMachineEventsRecent(context.Background(), store.ListMachineEventsRecentParams{MachineID: id, Limit: 1})
	if err != nil || len(evs) == 0 {
		return fmt.Sprintf("(no events: %v)", err)
	}
	return string(evs[0].Payload)
}

// terminalRun sends cmd over the browser terminal WebSocket and waits for want,
// with a timeout sized for a real guest rather than the dev stub.
func terminalRun(t *testing.T, fx cpFixture, session, cmd, want string, timeout time.Duration) string {
	t.Helper()
	c := dialBrowser(t, fx, session)
	defer c.Close(websocket.StatusNormalClosure, "")
	if f := readControlFrame(t, c); f.Type != guestwire.FrameHello {
		t.Fatalf("first frame = %q, want hello", f.Type)
	}
	writeBinary(t, c, cmd+"\n")
	got := readBinaryUntil(t, c, want, timeout)
	if !strings.Contains(got, want) {
		t.Fatalf("terminal: did not see %q; got %q", want, got)
	}
	return got
}

// mintPreview asks the control plane for a preview web-session URL and returns
// the status code, so the control plane's own allowlist can be asserted.
func mintPreview(t *testing.T, fx cpFixture, port int) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/machine/web-session?port=%d", fx.url, port), nil)
	req.Header.Set("X-Requested-By", "proteos")
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: fx.token})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("mint preview %d: %v", port, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestKindControlPlaneLifecycle is the K3 placement path against a real
// cluster: the control plane creates a machine on a kubernetes host from an
// image-only template, reaches it through the terminal gateway, stops it with a
// poweroff (never a hibernate), starts it cold, and destroys it.
func TestKindControlPlaneLifecycle(t *testing.T) {
	nodes, image := kindNodes(t)
	fx, svc := setupCPKind(t, nodes, image)
	ctx := context.Background()

	m, err := svc.Create(ctx, fx.userID, machine.CreateOptions{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	fx.machineID = machine.UUIDString(m.ID)
	fx.machinePgID = m.ID
	// On failure the machine is left for e2e-kind.sh's post-mortem dump; on
	// success Destroy below has already removed it.
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		t.Logf("test failed: leaving machine %s in the cluster for the post-mortem dump", fx.machineID)
	})

	waitCPStateFor(t, fx, machine.StateRunning, kindBootTimeout)
	sum := getSummary(t, fx)
	if sum.Backend != agentapi.DriverKubernetes {
		t.Errorf("summary backend = %q, want kubernetes", sum.Backend)
	}
	if sum.Boot == nil || *sum.Boot != agentapi.BootCold {
		t.Errorf("summary boot = %v, want cold", sum.Boot)
	}

	// Through the gateway, into a real PTY in a real pod: the session runs as the
	// unprivileged user, and the marker is computed so the PTY echo of the
	// command cannot satisfy the match.
	out := terminalRun(t, fx, "cp1", "id -un; echo kind-$((40+2))", "kind-42", 90*time.Second)
	if !strings.Contains(out, "dev") {
		t.Errorf("session does not run as `dev`: %q", out)
	}

	// Stop. On this backend the control plane must send a poweroff and move the
	// machine through stopping, and the result has no snapshot.
	if _, err := svc.Stop(ctx, fx.userID, fx.machinePgID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitCPStateFor(t, fx, machine.StateStopped, kindStopTimeout)
	sum = getSummary(t, fx)
	if sum.Snapshot != nil {
		t.Errorf("stopped kubernetes machine claims a snapshot: %+v", sum.Snapshot)
	}
	assertEventStates(t, fx, "stopping", "stopped")
	evs, err := fx.q.ListMachineEventsRecent(ctx, store.ListMachineEventsRecentParams{MachineID: fx.machinePgID, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if e.ToState != nil && *e.ToState == "hibernating" {
			t.Error("the control plane hibernated a kubernetes machine; it must poweroff")
		}
	}

	// Start again: a cold boot, and the terminal is back.
	if _, err := svc.Start(ctx, fx.userID, fx.machinePgID); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitCPStateFor(t, fx, machine.StateRunning, kindBootTimeout)
	terminalRun(t, fx, "cp2", "echo back-$((20+3))", "back-23", 90*time.Second)

	// Destroy is synchronous and removes the row.
	if err := svc.Destroy(ctx, fx.userID, fx.machinePgID, false); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if _, err := fx.q.GetMachineByUserID(ctx, fx.userID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("machine row after destroy: err=%v, want no rows", err)
	}
}

// TestKindControlPlanePreview is the machine-web preview proxy against a real
// machine, with BOTH allowlists in the path: the control plane's (which gates
// the mint) and the cluster-agent's (which gates the tunnel). The kind overlay
// narrows the agent's range so the two can be told apart.
func TestKindControlPlanePreview(t *testing.T) {
	nodes, image := kindNodes(t)
	fx, svc := setupCPKind(t, nodes, image)
	ctx := context.Background()

	m, err := svc.Create(ctx, fx.userID, machine.CreateOptions{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	fx.machineID = machine.UUIDString(m.ID)
	fx.machinePgID = m.ID
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("test failed: leaving machine %s in the cluster for the post-mortem dump", fx.machineID)
			return
		}
		_ = svc.Destroy(context.Background(), fx.userID, fx.machinePgID, true)
	})
	waitCPStateFor(t, fx, machine.StateRunning, kindBootTimeout)

	// Two in-guest servers: one inside the agent's allowlist, one outside it.
	start := func(port int, body string) string {
		return fmt.Sprintf(`nohup node -e "require('http').createServer((q,s)=>s.end('%s')).listen(%d,'127.0.0.1')" >/tmp/p%d.log 2>&1 &`, body, port, port)
	}
	terminalRun(t, fx, "pv",
		start(kindAgentPreviewMax, "preview-in-range")+" "+start(kindAgentPreviewOut, "preview-out-of-range")+
			fmt.Sprintf(` for i in $(seq 1 40); do (exec 3<>/dev/tcp/127.0.0.1/%d) 2>/dev/null && (exec 4<>/dev/tcp/127.0.0.1/%d) 2>/dev/null && break; sleep 0.5; done; echo up-$((6*7))`,
				kindAgentPreviewMax, kindAgentPreviewOut),
		"up-42", 90*time.Second)

	// 1. In range for both: mint → auth → proxy → the app inside the pod.
	reachPreview(t, fx, kindAgentPreviewMax, "preview-in-range")

	// 2. The control plane's allowlist: a system port and an impossible port are
	// refused at the mint, before anything is dialed.
	for _, port := range []int{int(agentapi.GuestWebPort), 70000} {
		if code, body := mintPreview(t, fx, port); code != http.StatusBadRequest {
			t.Errorf("mint for port %d = %d %q, want 400 from the control plane's allowlist", port, code, body)
		}
	}

	// 3. The agent's allowlist: the control plane admits 9000 (default range),
	// the agent refuses to tunnel to it, and the proxy reports that rather than
	// the app's body. This is the layering the plan asked to see end to end.
	code, _ := mintPreview(t, fx, kindAgentPreviewOut)
	if code != http.StatusOK {
		t.Fatalf("mint for port %d = %d, want 200 (the control plane's default range admits it)", kindAgentPreviewOut, code)
	}
	previewHost := fmt.Sprintf("m-%s-p%d.localhost", fx.machineID, kindAgentPreviewOut)
	_, body := mintPreview(t, fx, kindAgentPreviewOut)
	token := mustTokenParam(t, strings.TrimSpace(strings.SplitN(strings.SplitN(body, `"url":"`, 2)[1], `"`, 2)[0]))
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	authReq, _ := http.NewRequest(http.MethodGet, fx.url+"/__proteos/auth?token="+token, nil)
	authReq.Host = previewHost
	authResp, err := noFollow.Do(authReq)
	if err != nil {
		t.Fatal(err)
	}
	authResp.Body.Close()
	var cookie *http.Cookie
	for _, c := range authResp.Cookies() {
		if c.Name == "proteos_machine" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatalf("preview auth for port %d set no cookie (status %d)", kindAgentPreviewOut, authResp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodGet, fx.url+"/", nil)
	req.Host = previewHost
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	pbody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK || strings.Contains(string(pbody), "preview-out-of-range") {
		t.Fatalf("port %d is outside the AGENT's allowlist but the app answered (%d %q): the agent's PROTEOS_PREVIEW_PORT_MIN/MAX is not enforced",
			kindAgentPreviewOut, resp.StatusCode, pbody)
	}
}
