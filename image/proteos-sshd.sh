#!/bin/sh
# proteos-sshd — the machine pod's sshd sidecar entrypoint (K4).
#
# sshd has no supervisor in this image (there is no systemd; the guest agent is
# PID 1 of its own container), so the two things the sshd unit used to do before
# starting the daemon are done here instead:
#
#   1. Host keys. `ssh-keygen -A` writes into /etc/ssh, which is the container's
#      ephemeral rootfs — every restart would hand the user a different
#      fingerprint and a "host key changed" warning. Generating them on the
#      machine's own volume instead makes the identity survive restarts and
#      stop/start cycles, while a brand-new machine still gets a fresh one.
#      Only missing keys are created, so this is safe to re-run every start.
#   2. /run/sshd, sshd's privilege-separation directory, which the package's
#      postinst creates on a normal install and a container image does not keep.
#
# Then exec, so sshd is the container's PID 1 and receives SIGTERM directly.
set -eu

KEY_DIR="${PROTEOS_SSH_HOST_KEY_DIR:-/persist/ssh_host_keys}"

if ! mkdir -p "$KEY_DIR"; then
    echo "proteos-sshd: cannot create $KEY_DIR — is the persist volume mounted?" >&2
    exit 1
fi
chmod 0700 "$KEY_DIR"

for type in ed25519 ecdsa rsa; do
    key="$KEY_DIR/ssh_host_${type}_key"
    if [ ! -f "$key" ]; then
        echo "proteos-sshd: generating $type host key"
        ssh-keygen -q -t "$type" -f "$key" -N "" -C "proteos-machine"
    fi
done

mkdir -p /run/sshd
chmod 0755 /run/sshd

# -D: stay in the foreground (the container IS the supervisor).
# -e: log to stderr, so machine logs go where kubectl logs can read them.
exec /usr/sbin/sshd -D -e "$@"
