package kubernetes_test

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	api "github.com/tavon-ai/proteos/nodeagent/api"
	k8s "github.com/tavon-ai/proteos/nodeagent/internal/driver/kubernetes"
)

// Guest listener authentication (K5). The threat these tests are about: on a
// CNI that does not enforce NetworkPolicy, every machine's terminal is a plain
// TCP listener any pod can reach. The per-machine token is the control that
// does not depend on the CNI, so what matters is that it is minted per boot,
// reaches the guest only by Secret reference, is presented first on every dial,
// and leaves with the machine.

// pipeDialer records what the driver writes on a dial by routing it to a real
// loopback listener and handing the accepted side to the test. A real socket,
// not net.Pipe: the driver writes its preamble BEFORE returning from DialGuest,
// and an unbuffered pipe would block that write until the test reads — which it
// cannot do until DialGuest returns.
type pipeDialer struct {
	addrs  []string
	ln     net.Listener
	server chan net.Conn
}

func newPipeDialer(t *testing.T) *pipeDialer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	p := &pipeDialer{ln: ln, server: make(chan net.Conn, 8)}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			p.server <- conn
		}
	}()
	return p
}

func (p *pipeDialer) dial(_ context.Context, _, addr string) (net.Conn, error) {
	p.addrs = append(p.addrs, addr)
	return net.Dial("tcp", p.ln.Addr().String())
}

// firstLines reads n lines from the server side of the most recent dial.
func (p *pipeDialer) firstLines(t *testing.T, n int) []string {
	t.Helper()
	select {
	case conn := <-p.server:
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		br := bufio.NewReader(conn)
		var lines []string
		for i := 0; i < n; i++ {
			line, err := br.ReadString('\n')
			if err != nil {
				t.Fatalf("reading line %d from the dialed conn: %v", i+1, err)
			}
			lines = append(lines, strings.TrimSuffix(line, "\n"))
		}
		return lines
	case <-time.After(5 * time.Second):
		t.Fatal("the driver never dialed")
		return nil
	}
}

func TestGuestAuthSecretIsMintedPerBootAndReferencedNotInlined(t *testing.T) {
	d, cluster := newDriver(t, 0)
	ctx := context.Background()
	const id = "a5000000-0000-4000-8000-000000000001"

	if _, err := d.EnsureRunning(ctx, spec(id)); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}

	secret, err := cluster.CoreV1().Secrets(testNamespace).Get(ctx, "m-"+id+"-guest", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the machine has no guest secret: %v", err)
	}
	token1 := secret.StringData["token"]
	if len(token1) != 64 {
		t.Fatalf("token = %q, want 32 random bytes as hex", token1)
	}
	if secret.Labels["app.kubernetes.io/managed-by"] != "proteos" {
		t.Error("the guest secret does not carry the machine labels")
	}

	// The pod carries the token by REFERENCE only.
	pod, err := cluster.CoreV1().Pods(testNamespace).Get(ctx, "m-"+id, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, env := range pod.Spec.Containers[0].Env {
		if env.Name != "PROTEOS_GUEST_AUTH_TOKEN" {
			continue
		}
		found = true
		if env.Value != "" {
			t.Errorf("token is inlined in the pod spec (visible to anyone who can get pods): %q", env.Value)
		}
		if env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil ||
			env.ValueFrom.SecretKeyRef.Name != "m-"+id+"-guest" || env.ValueFrom.SecretKeyRef.Key != "token" {
			t.Errorf("token env does not reference the machine's secret: %+v", env.ValueFrom)
		}
	}
	if !found {
		t.Fatal("the guest container is not given PROTEOS_GUEST_AUTH_TOKEN")
	}
	for _, env := range pod.Spec.Containers[1].Env {
		if env.Name == "PROTEOS_GUEST_AUTH_TOKEN" {
			t.Error("the sshd sidecar was given the token; only the guest-agent needs it")
		}
	}

	// A running machine's token is NOT rotated by an idempotent ensure: the
	// guest read it at start and the driver must keep presenting the same one.
	if _, err := d.EnsureRunning(ctx, spec(id)); err != nil {
		t.Fatal(err)
	}
	secret, _ = cluster.CoreV1().Secrets(testNamespace).Get(ctx, "m-"+id+"-guest", metav1.GetOptions{})
	if secret.StringData["token"] != token1 {
		t.Fatal("an ensure against a running machine rotated its token under the guest")
	}

	// Stop + start is a new pod, and a new pod gets a new token.
	if err := d.Stop(ctx, id, "poweroff"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.EnsureRunning(ctx, spec(id)); err != nil {
		t.Fatal(err)
	}
	secret, _ = cluster.CoreV1().Secrets(testNamespace).Get(ctx, "m-"+id+"-guest", metav1.GetOptions{})
	if secret.StringData["token"] == token1 {
		t.Fatal("a new pod was started with the previous boot's token")
	}

	// Destroy takes the secret with it.
	if err := d.Destroy(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := cluster.CoreV1().Secrets(testNamespace).Get(ctx, "m-"+id+"-guest", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("guest secret survived Destroy: err=%v", err)
	}
}

func TestDialGuestPresentsTheTokenFirstOnEveryPort(t *testing.T) {
	d, cluster := newDriver(t, 0)
	pipes := newPipeDialer(t)
	d.WithDialer(pipes.dial)
	ctx := context.Background()
	const id = "a5000000-0000-4000-8000-000000000002"

	if _, err := d.EnsureRunning(ctx, spec(id)); err != nil {
		t.Fatal(err)
	}
	secret, err := cluster.CoreV1().Secrets(testNamespace).Get(ctx, "m-"+id+"-guest", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := api.GuestAuthTag + " " + secret.StringData["token"]

	// System ports: the auth line and nothing else before the protocol.
	for _, port := range []uint32{api.GuestTerminalPort, api.GuestWebPort, api.GuestSSHPort} {
		conn, err := d.DialGuest(ctx, id, port)
		if err != nil {
			t.Fatalf("DialGuest(%d): %v", port, err)
		}
		lines := pipes.firstLines(t, 1)
		conn.Close()
		if lines[0] != want {
			t.Errorf("port %d: first line = %q, want the auth preamble", port, lines[0])
		}
	}

	// A preview port: the auth line, THEN the preview preamble, so the guest's
	// gate sees its line and the forwarder behind it still sees its own.
	conn, err := d.DialGuest(ctx, id, 3000)
	if err != nil {
		t.Fatal(err)
	}
	lines := pipes.firstLines(t, 2)
	conn.Close()
	if lines[0] != want || lines[1] != "3000" {
		t.Errorf("preview dial wrote %q, want [auth preamble, \"3000\"]", lines)
	}
	if last := pipes.addrs[len(pipes.addrs)-1]; !strings.HasSuffix(last, ":1026") {
		t.Errorf("preview dial went to %s, want the guest preview port", last)
	}
}

func TestDialGuestReadsTheNewTokenAfterAReboot(t *testing.T) {
	d, cluster := newDriver(t, 0)
	pipes := newPipeDialer(t)
	d.WithDialer(pipes.dial)
	ctx := context.Background()
	const id = "a5000000-0000-4000-8000-000000000003"

	if _, err := d.EnsureRunning(ctx, spec(id)); err != nil {
		t.Fatal(err)
	}
	conn, err := d.DialGuest(ctx, id, api.GuestTerminalPort)
	if err != nil {
		t.Fatal(err)
	}
	first := pipes.firstLines(t, 1)[0]
	conn.Close()

	// Reboot: new pod, new token. The cached token belonged to the old pod and
	// must not be presented to the new one.
	if err := d.Stop(ctx, id, "poweroff"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.EnsureRunning(ctx, spec(id)); err != nil {
		t.Fatal(err)
	}
	secret, _ := cluster.CoreV1().Secrets(testNamespace).Get(ctx, "m-"+id+"-guest", metav1.GetOptions{})
	conn, err = d.DialGuest(ctx, id, api.GuestTerminalPort)
	if err != nil {
		t.Fatal(err)
	}
	second := pipes.firstLines(t, 1)[0]
	conn.Close()
	if second == first {
		t.Fatal("the driver presented the previous boot's token to the new pod")
	}
	if second != api.GuestAuthTag+" "+secret.StringData["token"] {
		t.Fatalf("presented %q, want the current secret", second)
	}
}

func TestDialGuestFailsClearlyWhenTheSecretIsGone(t *testing.T) {
	d, cluster := newDriver(t, 0)
	pipes := newPipeDialer(t)
	d.WithDialer(pipes.dial)
	ctx := context.Background()
	const id = "a5000000-0000-4000-8000-000000000004"

	if _, err := d.EnsureRunning(ctx, spec(id)); err != nil {
		t.Fatal(err)
	}
	if err := cluster.CoreV1().Secrets(testNamespace).Delete(ctx, "m-"+id+"-guest", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err := d.DialGuest(ctx, id, api.GuestTerminalPort)
	if err == nil || !strings.Contains(err.Error(), "guest secret") {
		t.Fatalf("err = %v, want one naming the missing guest secret rather than a dial that the guest hangs up on", err)
	}
	if len(pipes.addrs) != 0 {
		t.Error("the driver dialed the guest without a token to present")
	}
}

func TestGuestAuthCanBeDisabledForAnOlderImage(t *testing.T) {
	_, cluster := newDriver(t, 0)
	d := k8s.New(cluster, k8s.Config{Namespace: testNamespace, DisableGuestAuth: true})
	pipes := newPipeDialer(t)
	d.WithDialer(pipes.dial)
	ctx := context.Background()
	const id = "a5000000-0000-4000-8000-000000000005"

	if _, err := d.EnsureRunning(ctx, spec(id)); err != nil {
		t.Fatal(err)
	}
	if _, err := cluster.CoreV1().Secrets(testNamespace).Get(ctx, "m-"+id+"-guest", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("auth disabled, but a guest secret was written: err=%v", err)
	}
	pod, _ := cluster.CoreV1().Pods(testNamespace).Get(ctx, "m-"+id, metav1.GetOptions{})
	for _, env := range pod.Spec.Containers[0].Env {
		if env.Name == "PROTEOS_GUEST_AUTH_TOKEN" {
			t.Error("auth disabled, but the guest was given a token variable")
		}
	}
	joined := strings.Join(pod.Spec.Containers[0].ReadinessProbe.Exec.Command, " ")
	if strings.Contains(joined, api.GuestAuthTag) {
		t.Error("auth disabled, but the readiness probe presents a preamble the guest is not expecting")
	}

	conn, err := d.DialGuest(ctx, id, 3000)
	if err != nil {
		t.Fatal(err)
	}
	lines := pipes.firstLines(t, 1)
	conn.Close()
	if lines[0] != "3000" {
		t.Errorf("auth disabled: first line = %q, want only the preview preamble", lines[0])
	}

	// Destroy still removes a secret if one exists from before auth was turned off.
	if err := d.Destroy(ctx, id); err != nil {
		t.Fatal(err)
	}
}

// A sanity check on the contract the guest side implements independently: the
// two modules do not import each other, so the format is fixed in words in both
// and this pins the node-agent's half.
func TestGuestAuthPreambleFormat(t *testing.T) {
	got := api.GuestAuthPreamble("abc")
	if got != "PROTEOS-GUEST-AUTH abc\n" {
		t.Fatalf("preamble = %q", got)
	}
}
