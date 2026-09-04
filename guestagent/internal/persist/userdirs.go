// User-directory preparation, shared by BOTH persistence modes.
//
// These live outside mount_linux.go because dir mode needs them just as much as
// disk mode does, and dir mode is not Linux-only. That split was the bug: disk
// mode chowned the session user's home and workspace and seeded its dotfiles,
// dir mode did neither — so on the Kubernetes backend, where dir mode is the
// only mode and the agent runs as root while sessions run as `dev`, the user got
// a workspace it could not write to and an empty home. Keeping one
// implementation for both modes is what stops them drifting apart again.
package persist

import (
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
)

// claudeBinaryPath is where build-rootfs.sh bakes the Claude Code CLI. A var (not
// a const) so tests can point it at a fixture. The native binary's startup check
// looks for a launcher at ~/.local/bin/claude regardless of what is on PATH.
var claudeBinaryPath = "/usr/local/bin/claude"

// prepareUserDirs makes home and work exist, belong to the session user, and
// look like a home (dotfiles seeded, the Claude launcher linked). uid 0 means
// sessions run as root, so ownership is already right and the chowns are
// skipped. Every step is best-effort: a machine with an awkward home is far
// better than a machine that refuses to boot.
func prepareUserDirs(home, work string, uid, gid int) error {
	if err := ensureDirs(home, work); err != nil {
		return err
	}
	if uid == 0 {
		ensureClaudeLauncher(home, uid, gid)
		return nil
	}
	seedHome(home)
	if err := chownTreeIfNeeded(home, uid, gid); err != nil {
		slog.Warn("persist: chown home failed; the session user may not be able to write it", "err", err)
	}
	if err := chownTreeIfNeeded(work, uid, gid); err != nil {
		slog.Warn("persist: chown workspace failed; the session user may not be able to write it", "err", err)
	}
	ensureClaudeLauncher(home, uid, gid)
	return nil
}

// ensureClaudeLauncher makes ~/.local/bin/claude in the persisted home a symlink to
// the baked binary so Claude Code's installation health check passes without a
// per-$HOME `claude install`. Idempotent and best-effort: if Claude was not baked
// into this image (binary absent) or any fs step fails it logs and returns, never
// blocking the boot. When uid != 0 the created path components are lchown'd to the
// session user so it owns its own launcher.
func ensureClaudeLauncher(home string, uid, gid int) {
	if _, err := os.Stat(claudeBinaryPath); err != nil {
		return // Claude not baked into this image — nothing to link.
	}
	binDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		slog.Warn("persist: mkdir ~/.local/bin failed; claude launcher not ensured", "err", err)
		return
	}
	link := filepath.Join(binDir, "claude")
	if claudeLinkNeedsRepair(link) {
		_ = os.Remove(link) // drop a stale link/file; ENOENT (absent) is fine
		if err := os.Symlink(claudeBinaryPath, link); err != nil {
			slog.Warn("persist: symlink claude launcher failed", "err", err)
			return
		}
	}
	if uid != 0 {
		// Just the three new components, not a recursive walk: ~/.local may already
		// hold an arbitrarily large tree the user owns.
		for _, p := range []string{filepath.Join(home, ".local"), binDir, link} {
			if err := os.Lchown(p, uid, gid); err != nil {
				slog.Warn("persist: chown claude launcher path failed", "path", p, "err", err)
			}
		}
	}
}

// claudeLinkNeedsRepair reports whether link is absent, not a symlink, or points
// somewhere other than the baked binary — i.e. whether it must be (re)created.
func claudeLinkNeedsRepair(link string) bool {
	fi, err := os.Lstat(link)
	if err != nil {
		return true // absent or unreadable — (re)create
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		return true // a regular file/dir squatting the path — replace it
	}
	target, err := os.Readlink(link)
	return err != nil || target != claudeBinaryPath
}

// chownTreeIfNeeded recursively chowns root to uid:gid, but returns early when
// root already belongs to uid — the common steady-state case, where re-walking
// a large tree would only waste boot time. The top-dir owner is a reliable proxy
// because the tree is only ever chowned as a whole.
func chownTreeIfNeeded(root string, uid, gid int) error {
	if fi, err := os.Stat(root); err == nil {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) == uid {
			return nil
		}
	}
	return filepath.Walk(root, func(p string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Chown(p, uid, gid)
	})
}

// seedHome populates an empty home from /etc/skel (best-effort) so a brand-new
// disk gives the unprivileged user the usual .bashrc/.profile on first login.
// Does nothing if the home already has contents or /etc/skel is absent.
func seedHome(home string) {
	entries, err := os.ReadDir(home)
	if err != nil || len(entries) > 0 {
		return // unreadable or already populated — leave it alone
	}
	skel, err := os.ReadDir("/etc/skel")
	if err != nil {
		return
	}
	for _, e := range skel {
		if err := copyTree(filepath.Join("/etc/skel", e.Name()), filepath.Join(home, e.Name())); err != nil {
			slog.Warn("persist: seed skel entry failed", "entry", e.Name(), "err", err)
		}
	}
}

// copyTree copies src (file, dir, or symlink) to dst, recursively, preserving
// mode. Ownership is fixed up afterward by the chownTree pass in setupDiskBinds.
func copyTree(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(target, dst)
	case info.IsDir():
		if err := os.MkdirAll(dst, info.Mode().Perm()); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := copyTree(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
		return nil
	default:
		data, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, data, info.Mode().Perm())
	}
}
