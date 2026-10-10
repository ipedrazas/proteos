package kubernetes

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/ipedrazas/proteos/nodeagent/internal/driver"
)

// Guest listener authentication (K5).
//
// Every listener in a machine pod — terminal, code-server, preview, ssh — is a
// TCP socket on the pod IP, and the machine's NetworkPolicy is what keeps other
// pods out of it. That is a control the data path cannot observe: on a CNI that
// accepts NetworkPolicy objects without enforcing them, the objects exist, the
// preflight canary is the only thing that notices, and every machine's terminal
// is open to the cluster. This is the second, independent control. The driver
// mints a random token per machine boot, hands it to the guest-agent through a
// Secret, and writes it as the first line of every connection it opens; the
// guest closes anything that does not present it (guestagent/internal/authlisten).
//
// The token is per BOOT, not per machine: it is rotated whenever the pod is
// created, and read back from the Secret keyed by the pod's UID, so a stale
// cache entry can never be presented to a new pod.

// guestAuthEnv is the guest-agent variable the token arrives in. It is read and
// removed from the environment by the guest-agent at startup; the readiness
// probe (an exec, which inherits the container spec's env) uses it too.
const guestAuthEnv = "PROTEOS_GUEST_AUTH_TOKEN"

// guestSecretKey is the key inside the machine's Secret.
const guestSecretKey = "token"

func guestSecretName(machineID string) string { return resourceName(machineID) + "-guest" }

// guestToken is one machine's cached token and the pod it belongs to.
type guestToken struct {
	podUID types.UID
	token  string
}

// newGuestToken is 32 bytes of entropy as hex: comfortably beyond brute force
// on a listener that closes on the first wrong line, and free of every byte the
// one-line frame cares about.
func newGuestToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate guest token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// ensureGuestSecret writes a fresh token for the machine and returns it. Called
// on the pod CREATE path only: rotating the Secret under a running pod would
// leave the guest holding a token the driver no longer presents.
func (d *Driver) ensureGuestSecret(ctx context.Context, spec driver.VMSpec) error {
	token, err := newGuestToken()
	if err != nil {
		return err
	}
	name := guestSecretName(spec.MachineID)
	secrets := d.client.CoreV1().Secrets(d.cfg.Namespace)
	desired := &corev1.Secret{
		ObjectMeta: d.objectMeta(name, spec),
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{guestSecretKey: token},
	}

	existing, err := secrets.Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		if _, err := secrets.Create(ctx, desired, metav1.CreateOptions{}); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return nil // another ensure won the race and wrote its own token
			}
			return fmt.Errorf("create guest secret %s: %w", name, err)
		}
	case err != nil:
		return fmt.Errorf("get guest secret %s: %w", name, err)
	default:
		desired.ResourceVersion = existing.ResourceVersion
		if _, err := secrets.Update(ctx, desired, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("rotate guest secret %s: %w", name, err)
		}
	}
	// Whatever was cached belonged to the previous pod.
	d.tokens.Lock()
	delete(d.tokens.m, spec.MachineID)
	d.tokens.Unlock()
	return nil
}

// deleteGuestSecret is Destroy's half; NotFound is success.
func (d *Driver) deleteGuestSecret(ctx context.Context, machineID string) error {
	name := guestSecretName(machineID)
	err := d.client.CoreV1().Secrets(d.cfg.Namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete guest secret %s: %w", name, err)
	}
	d.tokens.Lock()
	delete(d.tokens.m, machineID)
	d.tokens.Unlock()
	return nil
}

// guestTokenFor returns the token the given pod was started with, reading the
// Secret once per pod and caching by pod UID: a dial is on the terminal's
// latency path and a preview proxies many connections, so one Secret read per
// connection would be the wrong trade, while keying by UID means a recreated
// pod (a new token) is never answered from the old entry.
func (d *Driver) guestTokenFor(ctx context.Context, machineID string, pod *corev1.Pod) (string, error) {
	d.tokens.Lock()
	cached, ok := d.tokens.m[machineID]
	d.tokens.Unlock()
	if ok && cached.podUID == pod.UID {
		return cached.token, nil
	}

	secret, err := d.client.CoreV1().Secrets(d.cfg.Namespace).
		Get(ctx, guestSecretName(machineID), metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("read guest secret for %s: %w", machineID, err)
	}
	token := string(secret.Data[guestSecretKey])
	if token == "" {
		// The fake clientset stores StringData without folding it into Data;
		// a real API server never returns StringData. Accept either.
		token = secret.StringData[guestSecretKey]
	}
	if token == "" {
		return "", fmt.Errorf("guest secret for %s has no %q key", machineID, guestSecretKey)
	}

	d.tokens.Lock()
	d.tokens.m[machineID] = guestToken{podUID: pod.UID, token: token}
	d.tokens.Unlock()
	return token, nil
}

// guestAuthEnabled reports whether machines are started with listener auth.
func (d *Driver) guestAuthEnabled() bool { return !d.cfg.DisableGuestAuth }
