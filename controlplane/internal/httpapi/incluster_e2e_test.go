//go:build kind_e2e

// Behind `kind_e2e` like kind_e2e_test.go, but a different shape of test: the
// control plane is NOT in this process. It is the one deployed by
// deploy/kubernetes/app-stack/ — its own Postgres, its own pod, dialling the
// cluster-agent by Service name over the pod network (K6) — and this test is a
// browser: a session cookie, the JSON API, the terminal WebSocket. What it
// proves is that the shipped manifests wire a working stack: the migration ran,
// the host row was seeded with driver=kubernetes, the agent's certificate pin
// and token came out of the agent's own Secret, the NetworkPolicies let the
// right pods talk, and a machine goes create → running → terminal → stop →
// start → destroy through the whole thing.
//
// The only thing it reaches around the API for is a session: the deployed
// stack has no IdP in the loop, so the test writes a user and a session row
// into the stack's database exactly as a login would, and presents the token
// as the cookie.
//
//	PROTEOS_E2E_CP_URL           http://127.0.0.1:<port>  (a port-forward to svc/proteos-web or the control plane)
//	PROTEOS_E2E_CP_DATABASE_URL  postgres://…            (a port-forward to the stack's Postgres)
//	PROTEOS_E2E_CP_ORIGIN        the stack's PROTEOS_BASE_URL (the WebSocket Origin it accepts)
package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ipedrazas/proteos/controlplane/internal/auth"
	"github.com/ipedrazas/proteos/controlplane/internal/httpapi"
	"github.com/ipedrazas/proteos/controlplane/internal/machine"
	"github.com/ipedrazas/proteos/controlplane/internal/session"
	"github.com/ipedrazas/proteos/controlplane/internal/store"
	guestwire "github.com/ipedrazas/proteos/guestagent/api"
	agentapi "github.com/ipedrazas/proteos/nodeagent/api"
)

type inClusterCP struct {
	url, origin, token string
	client             *http.Client
}

func inClusterStack(t *testing.T) inClusterCP {
	t.Helper()
	cpURL, dbURL := os.Getenv("PROTEOS_E2E_CP_URL"), os.Getenv("PROTEOS_E2E_CP_DATABASE_URL")
	if cpURL == "" || dbURL == "" {
		t.Skip("PROTEOS_E2E_CP_URL and PROTEOS_E2E_CP_DATABASE_URL unset: needs the deployed app stack (deploy/kubernetes/app-stack)")
	}
	origin := os.Getenv("PROTEOS_E2E_CP_ORIGIN")
	if origin == "" {
		origin = cpURL
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("stack database: %v", err)
	}
	t.Cleanup(pool.Close)
	q := store.New(pool)
	user, err := q.UpsertUser(ctx, store.UpsertUserParams{GithubUserID: 6161, Login: "k6-in-cluster-e2e", Email: "k6@x", AvatarUrl: ""})
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	token, err := session.NewManager(q, time.Hour).Create(ctx, user.ID)
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}
	return inClusterCP{url: strings.TrimSuffix(cpURL, "/"), origin: origin, token: token, client: &http.Client{Timeout: 30 * time.Second}}
}

func (c inClusterCP) do(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	var rdr io.Reader = http.NoBody
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.url+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Requested-By", "proteos") // the CSRF header every mutating route requires
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: c.token})
	resp, err := c.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (c inClusterCP) summary(t *testing.T, id string) (httpapi.MachineSummary, int) {
	t.Helper()
	code, b := c.do(t, http.MethodGet, "/api/machines/"+id, nil)
	var sum httpapi.MachineSummary
	if code == http.StatusOK {
		if err := json.Unmarshal(b, &sum); err != nil {
			t.Fatalf("decode summary: %v", err)
		}
	}
	return sum, code
}

func (c inClusterCP) waitState(t *testing.T, id string, want machine.State, timeout time.Duration) httpapi.MachineSummary {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last httpapi.MachineSummary
	for time.Now().Before(deadline) {
		sum, code := c.summary(t, id)
		if code == http.StatusOK {
			last = sum
			if sum.State == string(want) {
				return sum
			}
			if sum.State == string(machine.StateError) {
				reason := ""
				if sum.LastError != nil {
					reason = *sum.LastError
				}
				t.Fatalf("machine went to error while waiting for %q: %s", want, reason)
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("machine never reached %q in %s (last state %q)", want, timeout, last.State)
	return last
}

// terminal runs one command through the deployed gateway and returns the PTY
// output once want has appeared.
func (c inClusterCP) terminal(t *testing.T, sessionName, cmd, want string, timeout time.Duration) string {
	t.Helper()
	hdr := http.Header{}
	hdr.Set("Origin", c.origin)
	hdr.Set("Cookie", auth.SessionCookieName+"="+c.token)
	u := "ws" + strings.TrimPrefix(c.url, "http") + "/gw/terminal?session=" + sessionName
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatalf("terminal dial through the deployed stack: %v", err)
	}
	defer ws.Close(websocket.StatusNormalClosure, "")
	ws.SetReadLimit(8 << 20)
	if f := readControlFrame(t, ws); f.Type != guestwire.FrameHello {
		t.Fatalf("first frame = %q, want hello", f.Type)
	}
	writeBinary(t, ws, cmd+"\n")
	return readBinaryUntil(t, ws, want, timeout)
}

func TestInClusterAppStackLifecycle(t *testing.T) {
	cp := inClusterStack(t)

	// Unauthenticated is still 401 through the web tier — the proxy reaches the
	// API, and the API is not open.
	if resp, err := http.Get(cp.url + "/api/me"); err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /api/me without a session: %v / %v, want 401", resp, err)
	}

	code, b := cp.do(t, http.MethodPost, "/api/machines", map[string]any{"name": "k6-in-cluster"})
	if code != http.StatusAccepted {
		t.Fatalf("create: want 202, got %d: %s", code, b)
	}
	var created httpapi.MachineSummary
	if err := json.Unmarshal(b, &created); err != nil {
		t.Fatal(err)
	}
	id := created.ID
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("test failed: leaving machine %s for inspection", id)
			return
		}
		cp.do(t, http.MethodDelete, "/api/machines/"+id, nil)
	})
	if created.Backend != agentapi.DriverKubernetes {
		t.Errorf("backend = %q, want kubernetes: the host row was not seeded with the driver from controlplane.env", created.Backend)
	}

	sum := cp.waitState(t, id, machine.StateRunning, 4*time.Minute)
	if sum.GuestIP == nil || *sum.GuestIP == "" {
		t.Error("running machine reports no pod IP")
	}

	out := cp.terminal(t, "ic1", "id -un; echo k6-$((40+2))", "k6-42", 90*time.Second)
	if !strings.Contains(out, "dev") {
		t.Errorf("session does not run as `dev`: %q", out)
	}

	if code, b := cp.do(t, http.MethodPost, "/api/machines/"+id+"/stop", nil); code != http.StatusAccepted && code != http.StatusOK {
		t.Fatalf("stop: %d: %s", code, b)
	}
	sum = cp.waitState(t, id, machine.StateStopped, 2*time.Minute)
	if sum.Snapshot != nil {
		t.Error("a stopped kubernetes machine claims a snapshot")
	}

	if code, b := cp.do(t, http.MethodPost, "/api/machines/"+id+"/start", nil); code != http.StatusAccepted && code != http.StatusOK {
		t.Fatalf("start: %d: %s", code, b)
	}
	cp.waitState(t, id, machine.StateRunning, 4*time.Minute)
	cp.terminal(t, "ic2", "echo back-$((20+3))", "back-23", 90*time.Second)

	if code, b := cp.do(t, http.MethodDelete, "/api/machines/"+id, nil); code != http.StatusNoContent && code != http.StatusOK && code != http.StatusAccepted {
		t.Fatalf("destroy: %d: %s", code, b)
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if _, code := cp.summary(t, id); code == http.StatusNotFound {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("machine row still present two minutes after destroy")
		}
		time.Sleep(2 * time.Second)
	}
}
