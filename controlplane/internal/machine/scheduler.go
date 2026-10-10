package machine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/ipedrazas/proteos/controlplane/internal/store"
)

// ErrNoCapacity is returned by Create when no active host can take the requested
// machine — either none has enough spare capacity (TAV-37: multi-host
// foundation) or none runs a backend the template can boot on (K3). Callers wrap
// it with the reason; the API maps it to 503 no_capacity either way.
var ErrNoCapacity = errors.New("machine: no host with sufficient capacity") // 503

// chooseHost picks the host a new machine should be placed on (TAV-37: this is
// the first real scheduling decision — every machine used to go to a single
// hardcoded host). Candidates are the active hosts whose backend the template
// can boot on (K3). With exactly one such host — today's default,
// single-KVM-host deployments — it is returned directly with no agent
// round-trip, preserving the pre-TAV-37 behavior exactly. With more than one
// eligible host, each is queried for its current capacity (GET /v1/capacity) and
// the machine is placed on the host with the most free memory that can fit the
// request (both vcpus and mem must fit); ties break on host name for
// determinism. A host whose agent can't be reached is skipped rather than
// failing the whole placement decision — the point of having more than one
// host is that one being down doesn't stop new machines from scheduling.
func (s *Service) chooseHost(ctx context.Context, res Resources, t Template) (store.Host, error) {
	all, err := s.q.ListActiveHosts(ctx)
	if err != nil {
		return store.Host{}, fmt.Errorf("list active hosts: %w", err)
	}
	// K3: a host can only run a machine whose template carries an image for that
	// host's backend. Filtering here, before any capacity round-trip, is what
	// keeps an ext4-only template off a cluster and an image-only template off a
	// KVM box — the agent would otherwise reject the ensure and the machine would
	// land in error with a message about refs the user never chose.
	hosts := make([]store.Host, 0, len(all))
	for _, h := range all {
		if t.SupportsDriver(h.Driver) {
			hosts = append(hosts, h)
		}
	}
	if len(hosts) == 0 {
		if len(all) > 0 {
			return store.Host{}, fmt.Errorf("%w: template %q runs on %v hosts; none of the %d active hosts does",
				ErrNoCapacity, t.ID, t.SupportedDrivers(), len(all))
		}
		return store.Host{}, ErrNoCapacity
	}
	if len(hosts) == 1 {
		return hosts[0], nil
	}

	var best store.Host
	var bestFreeMem int
	found := false
	for _, h := range hosts {
		c, err := s.nodes.Capacity(ctx, h.ID)
		if err != nil {
			slog.Warn("scheduler: capacity query failed, skipping host", "host", h.Name, "err", err)
			continue
		}
		freeVcpus := c.TotalVcpus - c.UsedVcpus
		freeMem := c.TotalMemMiB - c.UsedMemMiB
		if freeVcpus < res.Vcpus || freeMem < res.MemMiB {
			continue
		}
		if !found || freeMem > bestFreeMem || (freeMem == bestFreeMem && h.Name < best.Name) {
			best, bestFreeMem, found = h, freeMem, true
		}
	}
	if !found {
		return store.Host{}, ErrNoCapacity
	}
	return best, nil
}
