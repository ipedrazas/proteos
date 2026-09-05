// Package config parses node-agent configuration from the environment. As with
// the control plane, all runtime knobs are resolved here into a typed Config so
// the rest of the daemon never reads os.Getenv ad hoc.
package config

import (
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	api "github.com/tavon-ai/proteos/nodeagent/api"
)

// Config is the fully-resolved node-agent configuration.
type Config struct {
	// Addr is the host:port the agent's HTTP API listens on. This should be a
	// private address reachable only by the control plane.
	Addr string

	// Token is the shared bearer token. The control plane must present the same
	// value; both sides compare in constant time. Required (non-empty).
	Token string

	// TLSCert / TLSKey, when both set, make the agent serve HTTPS instead of
	// plain HTTP. The channel carries per-machine LUKS volume keys and the
	// bearer token, so TLS is mandatory (TAV-27): startup fails without certs
	// unless InsecureHTTP explicitly opts out.
	TLSCert string
	TLSKey  string

	// InsecureHTTP (PROTEOS_AGENT_INSECURE_HTTP=1) permits serving plain HTTP
	// on a non-loopback Addr. Dev-only escape hatch; never set in production —
	// volume keys would transit cleartext. A loopback Addr is always allowed
	// without certs (that traffic never leaves the host).
	InsecureHTTP bool

	// DataDir is where per-machine state.json files and the IP allocator table
	// are persisted, so the agent re-attaches across restarts.
	DataDir string

	// Driver selects the VM backend: "dev" (process-backed stub) or
	// "firecracker" (linux-only, built behind the `firecracker` tag).
	Driver string

	// Subnet is the per-host guest subnet; the agent allocates the lowest free
	// host address from it. Gateway is its first usable address (.1).
	Subnet netip.Prefix

	// BootDelay is how long the dev driver pretends a boot takes before
	// transitioning creating→running. Ignored by the firecracker driver.
	BootDelay time.Duration

	// StubPath is the executable the dev driver runs as a stand-in VM process.
	// Empty means "resolve `sleep` from PATH".
	StubPath string

	// GuestAgentBin (PROTEOS_DEV_GUESTAGENT_BIN), when set, makes the dev driver
	// run the real guest agent per machine on a unix socket instead of the stub
	// (Phase 3): the whole terminal path then works on a Mac with no hypervisor.
	GuestAgentBin string

	// DevGuestWebBackend (PROTEOS_DEV_GUEST_WEB_BACKEND), when set, enables the
	// guest agent's Phase 8 web forward (code-server stand-in) in the dev driver,
	// pointing it at this address. In dev/e2e it is a stub HTTP+WS server, so the
	// code-server tunnel (DialGuest on the web port) round-trips with no real
	// code-server. Empty ⇒ terminal-only dev (no web listener). Unused by the
	// firecracker driver, which always runs the in-image code-server on 1025.
	DevGuestWebBackend string

	// GuestVsockPort is the fixed guest port the in-VM agent listens on; the
	// firecracker driver connects to it via the jailed vsock uds (Phase 3,
	// decision #3). Unused by the dev driver.
	GuestVsockPort int

	// PreviewPortMin / PreviewPortMax (PROTEOS_PREVIEW_PORT_MIN/MAX) bound the
	// previewable application ports the guest tunnel may reach (PP2). Reserved
	// system ports 1024/1025 stay rejected regardless; anything outside the range
	// is 400 before any dial. Defaults to the high range
	// (agentapi.DefaultPreviewPortMin..Max). The control plane reads the same env
	// names so the mint and the allowlist agree.
	PreviewPortMin uint32
	PreviewPortMax uint32

	// --- firecracker driver (Task 2.7); unused by the dev driver -------------

	// FirecrackerBin / JailerBin are absolute paths to the pinned binaries.
	FirecrackerBin string
	JailerBin      string

	// ChrootBaseDir is the jailer --chroot-base-dir; each VM gets
	// <ChrootBaseDir>/firecracker/<id>/root.
	ChrootBaseDir string

	// ImagesDir holds the pinned kernel + base rootfs referenced by kernel_ref
	// / rootfs_ref (resolved relative to this dir).
	ImagesDir string

	// JailUIDStart / JailUIDCount define the per-VM uid range the jailer drops
	// to (uid = JailUIDStart + offset derived from the machine).
	JailUIDStart int
	JailUIDCount int

	// --- Phase 4: persistent disk + hibernate/resume -------------------------

	// VolumesDir holds the per-machine LUKS volume container files
	// (<VolumesDir>/<machine-id>.luks). It lives OUTSIDE the jail tree so
	// prepareChroot never deletes it (decision #1). Unused by the dev driver.
	VolumesDir string

	// CryptsetupBin is the absolute path to cryptsetup, used to format/open/close
	// the machine volume. Unused by the dev driver.
	CryptsetupBin string

	// --- TAV-37: multi-host foundation ---------------------------------------

	// CapacityVcpus / CapacityMemMiB / CapacityDiskMiB (PROTEOS_AGENT_CAPACITY_
	// VCPUS/MEM_MIB/DISK_MIB) advertise this host's total resource shape to the
	// control plane's scheduler via GET /v1/capacity. Vcpus defaults to the
	// host's logical CPU count; mem/disk have no reliable portable
	// auto-detection, so they default conservatively and should be set
	// explicitly in production.
	CapacityVcpus   int
	CapacityMemMiB  int
	CapacityDiskMiB int

	// --- kubernetes driver (K1); unused by the dev/firecracker drivers -------

	// K8sNamespace (PROTEOS_K8S_NAMESPACE) is where machine pods, PVCs and
	// NetworkPolicies live. The cluster-agent's Role is scoped to it.
	K8sNamespace string

	// K8sAgentNamespace (PROTEOS_K8S_AGENT_NAMESPACE) is the namespace the
	// cluster-agent itself runs in — the ingress peer in every machine's
	// NetworkPolicy. Defaults to K8sNamespace. In-cluster it is read from the
	// downward API by the manifests, not guessed.
	K8sAgentNamespace string

	// K8sKubeconfig (PROTEOS_K8S_KUBECONFIG) selects an out-of-cluster client.
	// Empty ⇒ in-cluster (the production shape).
	K8sKubeconfig string

	// K8sStorageClass (PROTEOS_K8S_STORAGE_CLASS) backs machine volumes. Empty
	// ⇒ the cluster default. Because this backend cannot use the per-machine
	// LUKS volume key, an ENCRYPTED storage class is how encryption at rest is
	// restored — see SECURITY.md.
	K8sStorageClass string

	// K8sRuntimeClass (PROTEOS_K8S_RUNTIME_CLASS) is the sandboxed runtime
	// machine pods use; default "gvisor". Setting it to the EMPTY STRING (the
	// variable present but empty) is the explicit downgrade to the node's
	// default runtime — a shared kernel — and the driver says so loudly at
	// startup. Unsetting the variable keeps the safe default.
	K8sRuntimeClass string

	// K8sImagePullSecret (PROTEOS_K8S_IMAGE_PULL_SECRET) is attached to machine
	// pods pulling from a private registry.
	K8sImagePullSecret string

	// K8sNodeSelector (PROTEOS_K8S_NODE_SELECTOR, JSON object) pins machine pods
	// to a dedicated node pool, and bounds the nodes Capacity sums over.
	K8sNodeSelector map[string]string

	// K8sAgentPodLabels (PROTEOS_K8S_AGENT_POD_LABELS, JSON object) selects the
	// cluster-agent pod. It is the ONLY permitted ingress peer on the guest
	// ports, so without it machines are created unreachable (fail closed).
	K8sAgentPodLabels map[string]string

	// K8sClusterCIDRs (PROTEOS_K8S_CLUSTER_CIDRS, comma-separated) are the pod
	// and service networks machine egress must never reach, on top of the
	// built-in RFC1918/link-local/loopback deny list.
	K8sClusterCIDRs []string

	// K8sDNSNamespace (PROTEOS_K8S_DNS_NAMESPACE) holds the cluster DNS service;
	// machine egress always permits port 53 to it. Default kube-system.
	K8sDNSNamespace string

	// K8sCPURequestRatio (PROTEOS_K8S_CPU_REQUEST_RATIO, 0<r<=1, default 1.0)
	// scales a machine's CPU request relative to its limit. At the default a
	// machine needs a whole free core per vcpu to be scheduled — the closest
	// analogue to a Firecracker VM's fixed slice of a host, but more than a
	// small cluster can provide. Lowering it oversubscribes CPU (which is
	// compressible) while memory stays request == limit.
	K8sCPURequestRatio float64

	// K8sDefaultImage (PROTEOS_K8S_DEFAULT_IMAGE) is the guest image used when
	// an EnsureRequest carries no image_ref. It exists for the tracer bullet,
	// before K3 teaches the control plane to send one.
	K8sDefaultImage string

	// K8sGuestAuth (PROTEOS_K8S_GUEST_AUTH, default on; "0" disables) starts
	// every machine with a per-boot listener secret the agent presents first on
	// each tunnel connection, so NetworkPolicy is not the only thing between
	// another pod and a machine's terminal (K5). Off only for a guest image
	// that predates the preamble.
	K8sGuestAuth bool

	// K8sSkipNetpolProbe (PROTEOS_K8S_SKIP_NETPOL_PROBE=1) skips the startup
	// canary that proves the CNI enforces NetworkPolicy. Without the probe an
	// unenforced policy is invisible — the objects exist and do nothing.
	K8sSkipNetpolProbe bool

	// K8sNetpolProbeImage (PROTEOS_K8S_NETPOL_PROBE_IMAGE) is the canary's
	// listener image; empty ⇒ the driver's default (agnhost from registry.k8s.io).
	K8sNetpolProbeImage string

	// MgmtIfaces (PROTEOS_AGENT_MGMT_IFACES, comma-separated) are the host
	// interfaces allowed to reach SSH and the agent API through the firecracker
	// driver's fail-closed nftables input chain. The token "egress" resolves to
	// the default-route interface. Defaults to "egress,tailscale0": the
	// production control plane arrives over tailscale, so an egress-only list
	// firewalls it out (the July 2026 machine-create outage). Unused by the dev
	// driver.
	MgmtIfaces []string
}

// Load reads and validates configuration from the environment.
func Load() (*Config, error) {
	c := &Config{
		Addr:               getenv("PROTEOS_AGENT_ADDR", ":9090"),
		Token:              os.Getenv("PROTEOS_AGENT_TOKEN"),
		TLSCert:            os.Getenv("PROTEOS_AGENT_TLS_CERT"),
		TLSKey:             os.Getenv("PROTEOS_AGENT_TLS_KEY"),
		InsecureHTTP:       getenvBool("PROTEOS_AGENT_INSECURE_HTTP"),
		DataDir:            getenv("PROTEOS_AGENT_DATA_DIR", ".data/agent"),
		Driver:             getenv("PROTEOS_AGENT_DRIVER", "dev"),
		StubPath:           os.Getenv("PROTEOS_DEV_STUB"),
		GuestAgentBin:      os.Getenv("PROTEOS_DEV_GUESTAGENT_BIN"),
		DevGuestWebBackend: os.Getenv("PROTEOS_DEV_GUEST_WEB_BACKEND"),
		GuestVsockPort:     getenvInt("PROTEOS_GUEST_VSOCK_PORT", 1024),
		PreviewPortMin:     getenvUint32("PROTEOS_PREVIEW_PORT_MIN", api.DefaultPreviewPortMin),
		PreviewPortMax:     getenvUint32("PROTEOS_PREVIEW_PORT_MAX", api.DefaultPreviewPortMax),
		FirecrackerBin:     getenv("PROTEOS_FIRECRACKER_BIN", "/usr/local/bin/firecracker"),
		JailerBin:          getenv("PROTEOS_JAILER_BIN", "/usr/local/bin/jailer"),
		ChrootBaseDir:      getenv("PROTEOS_CHROOT_BASE_DIR", "/srv/jailer"),
		ImagesDir:          getenv("PROTEOS_AGENT_IMAGES_DIR", "/var/lib/proteos/images"),
		JailUIDStart:       getenvInt("PROTEOS_JAIL_UID_START", 100000),
		JailUIDCount:       getenvInt("PROTEOS_JAIL_UID_COUNT", 1000),
		VolumesDir:         getenv("PROTEOS_AGENT_VOLUMES_DIR", "/var/lib/proteos/volumes"),
		CryptsetupBin:      getenv("PROTEOS_CRYPTSETUP_BIN", "/usr/sbin/cryptsetup"),
		CapacityVcpus:      getenvInt("PROTEOS_AGENT_CAPACITY_VCPUS", runtime.NumCPU()),
		CapacityMemMiB:     getenvInt("PROTEOS_AGENT_CAPACITY_MEM_MIB", 8192),
		CapacityDiskMiB:    getenvInt("PROTEOS_AGENT_CAPACITY_DISK_MIB", 102400),
		K8sNamespace:       getenv("PROTEOS_K8S_NAMESPACE", "proteos-machines"),
		K8sAgentNamespace:  os.Getenv("PROTEOS_K8S_AGENT_NAMESPACE"),
		K8sKubeconfig:      os.Getenv("PROTEOS_K8S_KUBECONFIG"),
		K8sStorageClass:    os.Getenv("PROTEOS_K8S_STORAGE_CLASS"),
		// LookupEnv, not getenv: an explicitly EMPTY value is meaningful here —
		// it is the operator opting out of the sandboxed runtime.
		K8sRuntimeClass:    lookupenv("PROTEOS_K8S_RUNTIME_CLASS", "gvisor"),
		K8sImagePullSecret: os.Getenv("PROTEOS_K8S_IMAGE_PULL_SECRET"),
		K8sClusterCIDRs:    splitCSV(os.Getenv("PROTEOS_K8S_CLUSTER_CIDRS")),
		K8sDNSNamespace:    getenv("PROTEOS_K8S_DNS_NAMESPACE", "kube-system"),
		K8sDefaultImage:    os.Getenv("PROTEOS_K8S_DEFAULT_IMAGE"),
		K8sCPURequestRatio: getenvFloat("PROTEOS_K8S_CPU_REQUEST_RATIO", 1.0),
		// On unless explicitly "0": the safe default must survive an unset
		// variable, which is what a Deployment predating this setting has.
		K8sGuestAuth:        lookupenv("PROTEOS_K8S_GUEST_AUTH", "1") != "0",
		K8sSkipNetpolProbe:  getenvBool("PROTEOS_K8S_SKIP_NETPOL_PROBE"),
		K8sNetpolProbeImage: os.Getenv("PROTEOS_K8S_NETPOL_PROBE_IMAGE"),
	}

	if c.K8sCPURequestRatio <= 0 || c.K8sCPURequestRatio > 1 {
		return nil, fmt.Errorf("PROTEOS_K8S_CPU_REQUEST_RATIO must be in (0,1], got %v", c.K8sCPURequestRatio)
	}

	nodeSelector, err := getenvJSONMap("PROTEOS_K8S_NODE_SELECTOR")
	if err != nil {
		return nil, err
	}
	c.K8sNodeSelector = nodeSelector

	agentLabels, err := getenvJSONMap("PROTEOS_K8S_AGENT_POD_LABELS")
	if err != nil {
		return nil, err
	}
	c.K8sAgentPodLabels = agentLabels

	if (c.TLSCert == "") != (c.TLSKey == "") {
		return nil, fmt.Errorf("PROTEOS_AGENT_TLS_CERT and PROTEOS_AGENT_TLS_KEY must be set together")
	}
	if c.TLSCert == "" && !c.InsecureHTTP && !isLoopbackAddr(c.Addr) {
		return nil, fmt.Errorf("TLS is required on non-loopback PROTEOS_AGENT_ADDR %q (the agent API carries volume keys): set PROTEOS_AGENT_TLS_CERT/_KEY, or PROTEOS_AGENT_INSECURE_HTTP=1 for dev-only plain HTTP", c.Addr)
	}

	subnetStr := getenv("PROTEOS_AGENT_SUBNET", "172.30.0.0/24")
	prefix, err := netip.ParsePrefix(subnetStr)
	if err != nil {
		return nil, fmt.Errorf("PROTEOS_AGENT_SUBNET %q: %w", subnetStr, err)
	}
	c.Subnet = prefix.Masked()

	bootDelay := getenv("PROTEOS_DEV_BOOT_DELAY", "2s")
	d, err := time.ParseDuration(bootDelay)
	if err != nil {
		return nil, fmt.Errorf("PROTEOS_DEV_BOOT_DELAY %q: %w", bootDelay, err)
	}
	c.BootDelay = d

	if c.Token == "" {
		return nil, fmt.Errorf("PROTEOS_AGENT_TOKEN is required (shared bearer token with the control plane)")
	}

	if c.PreviewPortMin < 1 || c.PreviewPortMax > 65535 || c.PreviewPortMin > c.PreviewPortMax {
		return nil, fmt.Errorf("PROTEOS_PREVIEW_PORT_MIN/MAX invalid range: %d..%d", c.PreviewPortMin, c.PreviewPortMax)
	}

	c.MgmtIfaces = splitCSV(getenv("PROTEOS_AGENT_MGMT_IFACES", "egress,tailscale0"))
	if len(c.MgmtIfaces) == 0 {
		return nil, fmt.Errorf("PROTEOS_AGENT_MGMT_IFACES resolves to no interfaces — the fail-closed nftables input chain would firewall the control plane out")
	}
	return c, nil
}

// isLoopbackAddr reports whether a listen address binds only loopback
// (localhost or a 127.x/::1 address). An empty host (":9090") binds every
// interface and is NOT loopback.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// splitCSV splits a comma-separated value into trimmed, non-empty entries.
func splitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// lookupenv is getenv's "an empty value is a real value" sibling: it returns def
// only when the variable is ABSENT, so an operator can express "none" by setting
// it to the empty string.
func lookupenv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

// getenvJSONMap parses a JSON object of string→string (Kubernetes label sets).
// Absent or empty ⇒ nil, no error; malformed ⇒ a startup error, because a
// silently-ignored node selector or agent pod selector would place machines on
// the wrong nodes or leave them unreachable.
func getenvJSONMap(key string) (map[string]string, error) {
	raw := os.Getenv(key)
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("%s must be a JSON object of string labels: %w", key, err)
	}
	return m, nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvBool(key string) bool {
	switch os.Getenv(key) {
	case "1", "true", "yes":
		return true
	}
	return false
}

func getenvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func getenvFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func getenvUint32(key string, def uint32) uint32 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseUint(v, 10, 32); err == nil {
			return uint32(n)
		}
	}
	return def
}
