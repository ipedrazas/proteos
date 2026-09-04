package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	api "github.com/tavon-ai/proteos/nodeagent/api"
	k8s "github.com/tavon-ai/proteos/nodeagent/internal/driver/kubernetes"
	"github.com/tavon-ai/proteos/nodeagent/internal/driver/kubernetes/fakecluster"
	"github.com/tavon-ai/proteos/nodeagent/internal/httpapi"
)

// The HTTP layer is driver-agnostic by design — it only ever sees the Driver
// interface — but "by design" is a claim, not a guarantee. These tests run the
// same wire-contract assertions the dev driver gets (above) against the
// kubernetes driver, so a backend that breaks the contract fails here rather
// than in a cluster.
const k8sTestNamespace = "proteos-machines"

func newKubernetesServer(t *testing.T) *httptest.Server {
	t.Helper()
	cluster := fakecluster.New(fakecluster.Options{
		Namespace:     k8sTestNamespace,
		BootDelay:     fastBoot,
		FailBootImage: "ghcr.io/tavon-ai/proteos-guest:fail-boot",
		Nodes: []fakecluster.Node{
			{Name: "node-a", Vcpus: 8, MemMiB: 16384},
		},
	})
	drv := k8s.New(cluster, k8s.Config{
		Namespace:      k8sTestNamespace,
		AgentNamespace: "proteos-system",
		AgentPodLabels: map[string]string{"app.kubernetes.io/name": "proteos-cluster-agent"},
		CapacityTTL:    time.Nanosecond,
	})
	ts := httptest.NewServer(httpapi.New(testToken, drv).Handler())
	t.Cleanup(ts.Close)
	return ts
}

// ensureK8s is the kubernetes-flavoured EnsureRequest: an image_ref instead of
// the Firecracker kernel_ref/rootfs_ref pair.
func ensureK8s(image string) api.EnsureRequest {
	return api.EnsureRequest{Vcpus: 2, MemMiB: 2048, ImageRef: image, DiskMiB: 10240}
}

func TestKubernetesLifecycleOverTheWire(t *testing.T) {
	ts := newKubernetesServer(t)
	id := "aaaaaaaa-0000-4000-8000-00000000c001"
	const image = "ghcr.io/tavon-ai/proteos-guest:base-test"

	resp := do(t, ts, http.MethodPut, "/v1/machines/"+id, ensureK8s(image))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("ensure: want 202, got %d", resp.StatusCode)
	}
	var ens api.EnsureResponse
	json.NewDecoder(resp.Body).Decode(&ens)
	resp.Body.Close()
	if want := "k8s:" + k8sTestNamespace + "/m-" + id; ens.Handle != want {
		t.Fatalf("handle = %q, want %q", ens.Handle, want)
	}

	// creating → running, with the pod IP surfacing as the guest IP.
	st := waitState(t, ts, id, api.StateRunning)
	if st.GuestIP == "" {
		t.Error("running machine reports no guest IP")
	}
	if st.Boot != api.BootCold {
		t.Errorf("boot = %q, want cold: a pod has no RAM to restore", st.Boot)
	}
	if st.Snapshot.Present {
		t.Error("a pod-backed machine must never report a snapshot")
	}

	// Hibernate is accepted (the control plane sends it unconditionally until
	// K3) and behaves as a poweroff: stopped, cold, no snapshot.
	resp = do(t, ts, http.MethodPost, "/v1/machines/"+id+"/stop", api.StopRequest{Mode: api.StopModeHibernate})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("stop: want 202, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	st = waitState(t, ts, id, api.StateStopped)
	if st.Snapshot.Present {
		t.Error("hibernate claimed a snapshot the backend cannot take; the control plane would try to resume from it")
	}

	// A stopped machine is still listed — its volume is waiting for it.
	resp = do(t, ts, http.MethodGet, "/v1/machines", nil)
	var list api.ListResponse
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list.Machines) != 1 || list.Machines[0].MachineID != id {
		t.Fatalf("list = %+v", list.Machines)
	}

	// Start again, then destroy.
	resp = do(t, ts, http.MethodPut, "/v1/machines/"+id, ensureK8s(image))
	resp.Body.Close()
	waitState(t, ts, id, api.StateRunning)

	resp = do(t, ts, http.MethodDelete, "/v1/machines/"+id, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("destroy: want 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = do(t, ts, http.MethodGet, "/v1/machines/"+id, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get after destroy: want 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestKubernetesFailedBootSurfacesAsError(t *testing.T) {
	ts := newKubernetesServer(t)
	id := "bbbbbbbb-0000-4000-8000-00000000c002"

	resp := do(t, ts, http.MethodPut, "/v1/machines/"+id,
		ensureK8s("ghcr.io/tavon-ai/proteos-guest:fail-boot"))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("ensure: want 202, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	st := waitState(t, ts, id, api.StateError)
	if st.Reason == "" {
		t.Error("error state carries no reason, so the user sees no explanation")
	}
}

// A template with no image_ref cannot boot here. The agent must say so with a
// 4xx rather than accepting the machine and leaving it stuck in creating.
func TestKubernetesEnsureWithoutImageIsRejected(t *testing.T) {
	ts := newKubernetesServer(t)
	resp := do(t, ts, http.MethodPut, "/v1/machines/cccccccc-0000-4000-8000-00000000c003",
		api.EnsureRequest{Vcpus: 2, MemMiB: 2048, KernelRef: "vmlinux-x"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("ensure without an image: want 400 (the spec is unbootable here, "+
			"so retrying cannot help), got %d", resp.StatusCode)
	}
}

func TestKubernetesStopUnknownMachine(t *testing.T) {
	ts := newKubernetesServer(t)
	resp := do(t, ts, http.MethodPost, "/v1/machines/does-not-exist/stop", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("stop unknown: want 404, got %d", resp.StatusCode)
	}
}

// The guest tunnel must refuse a machine that is not running, exactly as it does
// for the dev driver: 409, before any dial.
func TestKubernetesGuestTunnelRejectsNotRunning(t *testing.T) {
	ts := newKubernetesServer(t)
	id := "dddddddd-0000-4000-8000-00000000c004"

	resp := do(t, ts, http.MethodPut, "/v1/machines/"+id,
		ensureK8s("ghcr.io/tavon-ai/proteos-guest:base-test"))
	resp.Body.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/machines/"+id+"/guest", nil)
	req.Header.Set(api.AuthHeader, api.BearerPrefix+testToken)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", api.UpgradeGuestProto)
	got, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer got.Body.Close()
	if got.StatusCode != http.StatusConflict {
		t.Fatalf("guest tunnel to a creating machine: want 409, got %d", got.StatusCode)
	}
}

func TestKubernetesCapacityOverTheWire(t *testing.T) {
	ts := newKubernetesServer(t)
	id := "eeeeeeee-0000-4000-8000-00000000c005"

	resp := do(t, ts, http.MethodPut, "/v1/machines/"+id,
		ensureK8s("ghcr.io/tavon-ai/proteos-guest:base-test"))
	resp.Body.Close()
	waitState(t, ts, id, api.StateRunning)

	resp = do(t, ts, http.MethodGet, "/v1/capacity", nil)
	defer resp.Body.Close()
	var cap api.CapacityResponse
	if err := json.NewDecoder(resp.Body).Decode(&cap); err != nil {
		t.Fatal(err)
	}
	if cap.TotalVcpus != 8 || cap.TotalMemMiB != 16384 {
		t.Errorf("cluster total = %d vcpus / %d MiB, want 8 / 16384", cap.TotalVcpus, cap.TotalMemMiB)
	}
	if cap.UsedVcpus != 2 || cap.UsedMemMiB != 2048 {
		t.Errorf("used = %d vcpus / %d MiB, want the one machine's 2 / 2048", cap.UsedVcpus, cap.UsedMemMiB)
	}
}
