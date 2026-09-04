package machine

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	agentapi "github.com/tavon-ai/proteos/nodeagent/api"
)

func validTemplates() []Template {
	return []Template{
		{ID: "base", Label: "Base", RootfsRef: "rootfs-base.ext4", Defaults: Resources{Vcpus: 2, MemMiB: 2048, DiskMiB: 10240}},
		{ID: "full", Label: "Full", RootfsRef: "rootfs-full.ext4", KernelRef: "vmlinux-7", Defaults: Resources{Vcpus: 4, MemMiB: 4096, DiskMiB: 20480}},
	}
}

func TestNewCatalog_DefaultsAndLookup(t *testing.T) {
	c, err := NewCatalog(validTemplates(), "vmlinux-6.1")
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	if c.Empty() {
		t.Fatal("catalog should not be empty")
	}
	if got := c.Default().ID; got != "base" {
		t.Fatalf("Default()=%q, want base (first entry)", got)
	}
	// base omits kernel_ref ⇒ filled from the default; full keeps its own.
	if got, _ := c.Get("base"); got.KernelRef != "vmlinux-6.1" {
		t.Fatalf("base kernel_ref=%q, want default vmlinux-6.1", got.KernelRef)
	}
	if got, _ := c.Get("full"); got.KernelRef != "vmlinux-7" {
		t.Fatalf("full kernel_ref=%q, want its own vmlinux-7", got.KernelRef)
	}
	if _, ok := c.Get("nope"); ok {
		t.Fatal("Get(nope) should be !ok")
	}
	if n := len(c.Templates()); n != 2 {
		t.Fatalf("Templates() len=%d, want 2", n)
	}
}

func TestNewCatalog_Validation(t *testing.T) {
	cases := []struct {
		name string
		in   []Template
		ker  string
	}{
		{"empty list", nil, "k"},
		{"blank id", []Template{{Label: "x", RootfsRef: "r", Defaults: Resources{1, 1, 1}}}, "k"},
		{"dup id", []Template{
			{ID: "a", RootfsRef: "r", Defaults: Resources{1, 1, 1}},
			{ID: "a", RootfsRef: "r2", Defaults: Resources{1, 1, 1}},
		}, "k"},
		{"no image of either kind", []Template{{ID: "a", Defaults: Resources{1, 1, 1}}}, "k"},
		{"no kernel anywhere", []Template{{ID: "a", RootfsRef: "r", Defaults: Resources{1, 1, 1}}}, ""},
		{"non-positive defaults", []Template{{ID: "a", RootfsRef: "r", Defaults: Resources{0, 1, 1}}}, "k"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewCatalog(tc.in, tc.ker); err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func TestSingleTemplateCatalog(t *testing.T) {
	c, err := SingleTemplateCatalog("rootfs.ext4", "vmlinux-6.1", "ghcr.io/tavon-ai/proteos-guest:base",
		Resources{Vcpus: 2, MemMiB: 2048, DiskMiB: 10240})
	if err != nil {
		t.Fatalf("SingleTemplateCatalog: %v", err)
	}
	d := c.Default()
	if d.ID != "base" || d.RootfsRef != "rootfs.ext4" || d.KernelRef != "vmlinux-6.1" {
		t.Fatalf("unexpected base template: %+v", d)
	}
	// K3: the same fallback template serves both backends when both refs are set.
	if d.ImageRef != "ghcr.io/tavon-ai/proteos-guest:base" {
		t.Fatalf("image_ref not carried: %+v", d)
	}
	if got := d.SupportedDrivers(); len(got) != 3 {
		t.Fatalf("want a template placeable on every backend, got %v", got)
	}
}

// K3: which refs a template carries decides which hosts it can be placed on. An
// ext4 rootfs is not an OCI image and neither backend can boot the other's, so
// this mapping is the whole of the scheduler's placement rule.
func TestTemplateSupportsDriver(t *testing.T) {
	fc := Template{ID: "fc", RootfsRef: "r.ext4", KernelRef: "k"}
	k8s := Template{ID: "k8s", ImageRef: "ghcr.io/x/guest:base"}
	both := Template{ID: "both", RootfsRef: "r.ext4", KernelRef: "k", ImageRef: "ghcr.io/x/guest:base"}

	cases := []struct {
		tmpl   Template
		driver string
		want   bool
	}{
		{fc, agentapi.DriverFirecracker, true},
		{fc, agentapi.DriverDev, true},
		{fc, agentapi.DriverKubernetes, false},
		{k8s, agentapi.DriverKubernetes, true},
		{k8s, agentapi.DriverFirecracker, false},
		{k8s, agentapi.DriverDev, false},
		{both, agentapi.DriverFirecracker, true},
		{both, agentapi.DriverKubernetes, true},
		// A kind this build has no meaning for supports nothing: the database is
		// ahead of the binary and guessing is worse than not scheduling.
		{both, "xen", false},
	}
	for _, tc := range cases {
		if got := tc.tmpl.SupportsDriver(tc.driver); got != tc.want {
			t.Errorf("template %q SupportsDriver(%q) = %v, want %v", tc.tmpl.ID, tc.driver, got, tc.want)
		}
	}

	if got := k8s.SupportedDrivers(); len(got) != 1 || got[0] != agentapi.DriverKubernetes {
		t.Errorf("k8s-only template drivers = %v", got)
	}
}

// An image-only template needs no kernel: there is nothing to boot it with on a
// cluster, and requiring one would force operators to invent a kernel ref for a
// Kubernetes-only fleet.
func TestCatalogAcceptsImageOnlyTemplateWithoutKernel(t *testing.T) {
	c, err := NewCatalog([]Template{{
		ID: "base", ImageRef: "ghcr.io/x/guest:base", Defaults: Resources{1, 1024, 5120},
	}}, "")
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	if d := c.Default(); d.KernelRef != "" {
		t.Fatalf("no kernel should have been filled in: %+v", d)
	}
}

func TestLoadCatalogFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "templates.json")
	body := `{"templates":[
		{"id":"go","label":"Go development","rootfs_ref":"rootfs-go.ext4","defaults":{"vcpus":2,"mem_mib":2048,"disk_mib":10240}},
		{"id":"full","label":"Full","rootfs_ref":"rootfs-full.ext4","defaults":{"vcpus":4,"mem_mib":4096,"disk_mib":20480}}
	]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadCatalogFile(path, "vmlinux-6.1")
	if err != nil {
		t.Fatalf("LoadCatalogFile: %v", err)
	}
	if c.Default().ID != "go" {
		t.Fatalf("default=%q, want go", c.Default().ID)
	}
	if got, _ := c.Get("go"); got.KernelRef != "vmlinux-6.1" {
		t.Fatalf("go kernel_ref=%q, want filled default", got.KernelRef)
	}

	if _, err := LoadCatalogFile(filepath.Join(dir, "missing.json"), "k"); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestResourceLimits(t *testing.T) {
	l := NewResourceLimits(8, 16384, 51200)
	if l.Empty() {
		t.Fatal("configured limits should not be Empty()")
	}
	if (ResourceLimits{}).Empty() == false {
		t.Fatal("zero limits should be Empty()")
	}
	// Fixed floors.
	if l.Vcpus.Min != 1 || l.MemMiB.Min != 1024 || l.DiskMiB.Min != 5120 {
		t.Fatalf("unexpected floors: %+v", l)
	}
	// In-range passes.
	if err := l.Validate(Resources{Vcpus: 4, MemMiB: 4096, DiskMiB: 20480}); err != nil {
		t.Fatalf("in-range should pass: %v", err)
	}
	// Each dimension out of range yields InvalidResourcesError naming it.
	cases := []struct {
		name string
		r    Resources
		want string
	}{
		{"vcpus high", Resources{Vcpus: 9, MemMiB: 4096, DiskMiB: 20480}, "vcpus must be 1..8"},
		{"vcpus low", Resources{Vcpus: 0, MemMiB: 4096, DiskMiB: 20480}, "vcpus must be 1..8"},
		{"mem high", Resources{Vcpus: 2, MemMiB: 99999, DiskMiB: 20480}, "mem_mib must be 1024..16384"},
		{"disk low", Resources{Vcpus: 2, MemMiB: 4096, DiskMiB: 100}, "disk_mib must be 5120..51200"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := l.Validate(tc.r)
			var ire InvalidResourcesError
			if !errors.As(err, &ire) {
				t.Fatalf("err=%v, want InvalidResourcesError", err)
			}
			if ire.Detail != tc.want {
				t.Fatalf("detail=%q, want %q", ire.Detail, tc.want)
			}
		})
	}
}

func TestParseResources(t *testing.T) {
	r := parseResources([]byte(`{"vcpus":4,"mem_mib":4096,"disk_mib":20480}`))
	if r.Vcpus != 4 || r.MemMiB != 4096 || r.DiskMiB != 20480 {
		t.Fatalf("parseResources got %+v", r)
	}
	if z := parseResources(nil); z != (Resources{}) {
		t.Fatalf("nil spec should be zero, got %+v", z)
	}
	if z := parseResources([]byte("not json")); z != (Resources{}) {
		t.Fatalf("malformed spec should be zero, got %+v", z)
	}
}
