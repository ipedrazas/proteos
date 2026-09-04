//go:build linux

package persist

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// mountDisk waits for the block device to appear (≤deviceWaitTimeout), preens it
// with fsck, and mounts it ext4 at mountpoint. If it is already mounted there
// (e.g. a re-run), it is treated as success.
func mountDisk(device, mountpoint string, wait time.Duration) error {
	if err := waitForDevice(device, wait); err != nil {
		return err
	}
	if err := os.MkdirAll(mountpoint, 0o700); err != nil {
		return fmt.Errorf("mkdir mountpoint: %w", err)
	}
	if alreadyMounted(mountpoint) {
		slog.Info("persist: device already mounted", "mountpoint", mountpoint)
		return nil
	}
	// fsck -p (preen) is belt-and-braces: ext4 replays its journal on mount, so a
	// missing fsck binary or a transient fsck hiccup must NOT cost us persistence.
	// Only a clearly-uncorrectable filesystem (exit ≥ 4) blocks the mount.
	preen(device)
	if err := unix.Mount(device, mountpoint, "ext4", 0, ""); err != nil {
		return fmt.Errorf("mount %s -> %s: %w", device, mountpoint, err)
	}
	return nil
}

// setupDiskBinds ensures home/ + workspace/ on the mounted disk and bind-mounts
// them over homeTarget (the session user's $HOME, e.g. /home/dev — or /root in
// the legacy root case) and /workspace so $HOME and the workspace live on the
// disk (decision #7). When uid != 0 the persisted home + workspace are chowned
// to the unprivileged session user so its shells can write them, and a fresh
// (empty) home is seeded from /etc/skel for a sane first login.
func setupDiskBinds(mount, homeTarget string, uid, gid int) error {
	homeSrc := filepath.Join(mount, "home")
	workSrc := filepath.Join(mount, "workspace")
	if err := ensureDirs(homeTarget, "/workspace"); err != nil {
		return err
	}
	// The bind SOURCES get the shared preparation — created, owned by the
	// session user, dotfiles seeded, Claude launcher linked — which is exactly
	// what dir mode needs too, so both modes now call the same code.
	if err := prepareUserDirs(homeSrc, workSrc, uid, gid); err != nil {
		return err
	}
	// Claude Code's startup health check nags `claude install to repair` unless a
	// launcher exists at ~/.local/bin/claude. ProteOS bakes the binary to
	// /usr/local/bin/claude with no per-$HOME install (build-rootfs.sh), and the
	// persistent home is bind-mounted over the rootfs home, so the launcher must be
	// ensured here on the live disk source — every boot, so it self-heals homes
	// that predate this fix. No-op when Claude was not baked into the image.
	ensureClaudeLauncher(homeSrc, uid, gid)
	// sshd's host keys must survive across this machine's own stop/start cycles
	// (the rootfs is reset to the golden image on every cold boot; only /persist
	// survives) or every reboot greets the user with a "host key changed"
	// warning. Symlinking /etc/ssh's host key files onto the persist disk (best-
	// effort, self-healing every boot) makes ssh-keygen -A (run by the sshd unit's
	// ExecStartPre) write through to disk the first time and skip regeneration on
	// every boot after. A brand-new machine still gets a fresh identity, by
	// design — only ITS OWN disk carries these keys forward.
	ensureSSHHostKeys(mount)
	if err := bindMount(homeSrc, homeTarget); err != nil {
		return fmt.Errorf("bind home: %w", err)
	}
	if err := bindMount(workSrc, "/workspace"); err != nil {
		return fmt.Errorf("bind workspace: %w", err)
	}
	return nil
}

// sshDir is where sshd (baked by image/build-rootfs.sh) reads its host keys
// from; a var so tests can point it at a fixture.
var sshDir = "/etc/ssh"

// sshHostKeyFiles are the host key (+ public half) filenames ssh-keygen -A
// creates. Listing them explicitly (rather than globbing at runtime) keeps this
// self-contained and matches every OpenSSH version's -A output.
var sshHostKeyFiles = []string{
	"ssh_host_rsa_key", "ssh_host_rsa_key.pub",
	"ssh_host_ecdsa_key", "ssh_host_ecdsa_key.pub",
	"ssh_host_ed25519_key", "ssh_host_ed25519_key.pub",
}

// ensureSSHHostKeys makes each host key file under sshDir a symlink into
// mount/ssh_host_keys, migrating any pre-existing regular file there first (the
// image ships no host keys — extract-only package installs skip openssh-
// server's postinst, see build-rootfs.sh's install_git doc comment for why —
// but this also rescues a key written by a boot that raced ahead of this step).
// ssh-keygen -A (run by the sshd unit's ExecStartPre) then creates any still-
// missing key by writing straight through the symlink to disk; a later boot
// finds the symlink already pointing at an existing file and ssh-keygen -A
// skips it, so the identity — and the fingerprint ssh clients pin — survives
// this machine's own stop/start cycles. Best-effort: any failure just costs
// host-key persistence (sshd still generates keys fine on the ephemeral
// rootfs), never the boot.
func ensureSSHHostKeys(mount string) {
	keysDir := filepath.Join(mount, "ssh_host_keys")
	if err := os.MkdirAll(keysDir, 0o700); err != nil {
		slog.Warn("persist: mkdir ssh host key dir failed; keys will not survive reboot", "err", err)
		return
	}
	for _, name := range sshHostKeyFiles {
		target := filepath.Join(sshDir, name)
		persisted := filepath.Join(keysDir, name)

		if link, err := os.Readlink(target); err == nil && link == persisted {
			continue // already wired up
		}

		if fi, err := os.Lstat(target); err == nil && fi.Mode()&os.ModeSymlink == 0 {
			// A real file (freshly generated by a boot that raced ahead of us, or a
			// base image that did run postinst) — rescue it onto the persist disk
			// rather than discarding it, unless persist already holds one (keep the
			// disk's copy; it is the one continuity depends on).
			if _, err := os.Stat(persisted); os.IsNotExist(err) {
				if err := os.Rename(target, persisted); err != nil {
					slog.Warn("persist: move ssh host key to disk failed", "file", name, "err", err)
					continue
				}
			}
		}

		_ = os.Remove(target) // drop the file/stale-symlink we just handled; ENOENT is fine
		if err := os.Symlink(persisted, target); err != nil {
			slog.Warn("persist: symlink ssh host key failed", "file", name, "err", err)
		}
	}
}

func bindMount(src, dst string) error {
	return unix.Mount(src, dst, "", unix.MS_BIND, "")
}

// waitForDevice blocks until path exists or timeout elapses.
func waitForDevice(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("device %s did not appear within %s", path, timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// alreadyMounted reports whether mountpoint is a mount point, by comparing its
// device id with its parent's.
func alreadyMounted(mountpoint string) bool {
	var st, parent unix.Stat_t
	if err := unix.Stat(mountpoint, &st); err != nil {
		return false
	}
	if err := unix.Stat(filepath.Dir(mountpoint), &parent); err != nil {
		return false
	}
	return st.Dev != parent.Dev
}

// preen runs fsck -p best-effort: it logs the outcome but never blocks the
// mount. fsck exit codes: 0 clean, 1 errors corrected, 2 corrected+reboot,
// 4 errors left uncorrected, 8 operational error, 16 usage. A missing binary or
// a transient failure is logged and ignored (the journal replay on mount, and
// the mount itself, are the real safety net).
func preen(device string) {
	if _, err := exec.LookPath("fsck"); err != nil {
		slog.Warn("persist: fsck not found; skipping preen (relying on ext4 journal)", "device", device)
		return
	}
	cmd := exec.Command("fsck", "-p", device)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err := cmd.Run()
	if err == nil {
		return
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		switch ee.ExitCode() {
		case 1, 2:
			slog.Info("persist: fsck corrected errors", "device", device, "code", ee.ExitCode())
		default:
			slog.Warn("persist: fsck reported uncorrected issues; attempting mount anyway",
				"device", device, "code", ee.ExitCode())
		}
		return
	}
	slog.Warn("persist: fsck could not run; attempting mount anyway", "device", device, "err", err)
}
