package kubernetes

import (
	"context"
	"fmt"
	"net"
	"strconv"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	api "github.com/tavon-ai/proteos/nodeagent/api"
	"github.com/tavon-ai/proteos/nodeagent/internal/driver"
)

// DialGuest opens a byte stream to the machine's in-guest agent by dialing the
// pod IP directly. The cluster-agent runs inside the cluster, so pod-to-pod
// routing is all this needs — no SPDY port-forward through the API server, and
// no extra hop in the terminal's latency path. The returned conn is spliced 1:1
// to the control-plane gateway by the node-agent's HTTP layer, which never
// parses what flows through it.
//
// Preview application ports keep the guest-agent's protocol contract: connect to
// GuestPreviewPort and write the target loopback port as a one-line preamble.
// That preamble is a guest-agent contract, not a vsock artefact, so it is
// identical on this backend.
func (d *Driver) DialGuest(ctx context.Context, machineID string, port uint32) (net.Conn, error) {
	pod, err := d.client.CoreV1().Pods(d.cfg.Namespace).
		Get(ctx, resourceName(machineID), metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, driver.ErrUnknownMachine
		}
		return nil, fmt.Errorf("get pod %s: %w", resourceName(machineID), err)
	}
	if pod.Status.PodIP == "" {
		return nil, fmt.Errorf("machine %s has no pod IP yet", machineID)
	}
	if !guestReady(pod) {
		return nil, fmt.Errorf("machine %s: guest agent is not ready", machineID)
	}
	// Inbound SSH lives in its own container, which is ready a little later than
	// the guest agent on a machine's first boot (it generates the host keys). A
	// machine is deliberately "running" as soon as the GUEST is ready — a broken
	// sshd must not cost a user their terminal — so this is the only place that
	// can tell an ssh caller "not yet" rather than handing back a connection the
	// guest's forward closes the moment it fails to reach sshd.
	if port == api.GuestSSHPort && !sshdReady(pod) {
		return nil, fmt.Errorf("machine %s: sshd is not ready yet", machineID)
	}

	if port == 0 {
		port = api.GuestTerminalPort
	}
	target, preamble := port, ""
	if !api.IsSystemGuestPort(port) {
		target, preamble = api.GuestPreviewPort, api.PreviewPreamble(port)
	}

	// The listener auth line goes FIRST, on every port (K5): the guest closes a
	// connection that opens with anything else, so the preview preamble — and
	// everything the gateway sends after we return — has to come behind it. Read
	// before the dial so a Secret that has gone missing is reported as such
	// rather than as the guest hanging up on us.
	if d.guestAuthEnabled() {
		token, err := d.guestTokenFor(ctx, machineID, pod)
		if err != nil {
			return nil, err
		}
		preamble = api.GuestAuthPreamble(token) + preamble
	}

	addr := net.JoinHostPort(pod.Status.PodIP, strconv.FormatUint(uint64(target), 10))
	dialCtx, cancel := context.WithTimeout(ctx, d.cfg.DialTimeout)
	defer cancel()
	conn, err := d.dial(dialCtx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial guest %s: %w", addr, err)
	}
	if preamble != "" {
		if _, err := conn.Write([]byte(preamble)); err != nil {
			conn.Close()
			return nil, fmt.Errorf("write guest preamble: %w", err)
		}
	}
	return conn, nil
}
