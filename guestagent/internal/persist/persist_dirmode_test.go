package persist

import (
	"os"
	"path/filepath"
	"testing"
)

// Dir mode is the Kubernetes backend's only persistence mode, and there the
// guest-agent runs as root while PTY sessions run as an unprivileged user. It
// therefore has to do everything disk mode does — create the home and workspace,
// hand them to the session user, and report where they are — or the user gets a
// workspace it cannot write to and a home that vanishes with the container.
func TestDirModePreparesHomeAndWorkspace(t *testing.T) {
	dir := t.TempDir()

	p, err := Setup(Config{
		Dir:       dir,
		Version:   "test",
		RunAsUID:  os.Getuid(), // chown to ourselves: the only uid a test may use
		RunAsGID:  os.Getgid(),
		RunAsUser: "dev",
	})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	defer p.Close()

	if p.Mode() != "dir" {
		t.Fatalf("mode = %q, want dir", p.Mode())
	}

	home, work := p.Home(), p.Workspace()
	if home != filepath.Join(dir, "home") {
		t.Errorf("Home() = %q, want <dir>/home", home)
	}
	if work != filepath.Join(dir, "workspace") {
		t.Errorf("Workspace() = %q, want <dir>/workspace", work)
	}

	// Both must exist and be writable BY THE SESSION USER. Writing as the test
	// process is the closest a non-root test can get to that, and it is exactly
	// the operation that failed in a pod: `bash: /persist/workspace/x:
	// Permission denied`.
	for _, d := range []string{home, work} {
		info, err := os.Stat(d)
		if err != nil {
			t.Fatalf("stat %s: %v", d, err)
		}
		if !info.IsDir() {
			t.Fatalf("%s is not a directory", d)
		}
		probe := filepath.Join(d, "writable-probe")
		if err := os.WriteFile(probe, []byte("ok"), 0o644); err != nil {
			t.Errorf("session user cannot write %s: %v", d, err)
		}
		_ = os.Remove(probe)
	}

	// The shell env must point HOME at the persisted home, not the image's.
	var sawHome bool
	for _, kv := range p.ShellEnv() {
		if kv == "HOME="+home {
			sawHome = true
		}
	}
	if !sawHome {
		t.Errorf("ShellEnv does not set HOME to the persisted home: %v", p.ShellEnv())
	}
}
