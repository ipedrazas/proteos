#!/usr/bin/env bash
# verify-guest-image.sh — acceptance check for the OCI guest image (K0).
#
# The ext4 image is verified by loop-mounting it and inspecting the tree
# (verify-phase5/7/8-rootfs.sh). An OCI image cannot be checked that way and does
# not need to be: it can simply be RUN. So this script boots the image the way a
# machine pod does — guest-agent as PID 1, TCP listeners, a dir-mode persist
# volume, a memory-backed /run/proteos — and asserts the guest surface actually
# answers:
#
#   1. GET /healthz on 1024                     (the pod readiness probe)
#   2. GET /info                                (persist is in DIR mode, not degraded)
#   3. GET /terminal WebSocket upgrade          (a PTY session really starts)
#   4. the port-preview forward on 1026         (preamble → in-container backend)
#   5. the baked platform layer                 (claude/git/gh/code-server/dev user)
#   6. /etc/proteos-release                     (provenance is stamped)
#
# Usage:
#   image/verify-guest-image.sh [--image ghcr.io/tavon-ai/proteos-guest:base-dev]
#                               [--build] [--template base] [--keep]
#
# --build builds the image from image/guest.Dockerfile first (context = repo
# root). --keep leaves the container running for poking at.
#
# Requires: docker, python3, curl.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

log() { printf '\e[1;34m[verify-guest]\e[0m %s\n' "$*"; }
ok() { printf '\e[1;32m[ ok ]\e[0m %s\n' "$*"; }
die() {
  printf '\e[1;31m[fail]\e[0m %s\n' "$*" >&2
  exit 1
}

TEMPLATE="base"
IMAGE=""
BUILD=0
KEEP=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --image) IMAGE=$2; shift 2 ;;
    --template) TEMPLATE=$2; shift 2 ;;
    --build) BUILD=1; shift ;;
    --keep) KEEP=1; shift ;;
    -h | --help) sed -n '2,25p' "$0"; exit 0 ;;
    *) die "unknown arg: $1" ;;
  esac
done
IMAGE="${IMAGE:-proteos-guest:${TEMPLATE}-verify}"

command -v docker >/dev/null || die "docker is required"
command -v python3 >/dev/null || die "python3 is required (WebSocket + preview probes)"
command -v curl >/dev/null || die "curl is required"

if [[ $BUILD -eq 1 ]]; then
  log "building $IMAGE (template=$TEMPLATE)"
  docker build -f "$SCRIPT_DIR/guest.Dockerfile" \
    --build-arg "TEMPLATE=$TEMPLATE" \
    --build-arg "GUESTAGENT_VERSION=ga-$(git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null || echo nogit)" \
    -t "$IMAGE" "$REPO_ROOT" || die "docker build failed"
fi

CID=""
# A named volume, not a host bind mount: the guest keeps a SQLite machine.db on
# the persist root, and Docker Desktop's bind mounts do not support the POSIX
# locks SQLite needs (the DB opens "readonly" and persistence silently degrades
# to ephemeral). A volume lives on a real filesystem inside the VM, which is what
# a PVC is too.
VOLUME="proteos-guest-verify-$$"
cleanup() {
  if [[ -n $CID ]]; then
    if [[ $KEEP -eq 1 ]]; then
      log "leaving container $CID running (--keep); remove it with: docker rm -f $CID && docker volume rm $VOLUME"
      return 0
    fi
    docker rm -f "$CID" >/dev/null 2>&1 || true
  fi
  docker volume rm -f "$VOLUME" >/dev/null 2>&1 || true
  return 0
}
trap cleanup EXIT

# Boot it the way a machine pod does. The capability set mirrors the machine
# pod's securityContext (K-plan decision 6): drop everything, then add back only
# what the guest-agent's root-agent / non-root-`dev` split needs — it chowns the
# tmpfs env dir and the persist tree to the session user and spawns PTYs as that
# user. Dropping DAC_OVERRIDE/CHOWN as well makes persistence silently degrade to
# ephemeral, which is exactly the kind of drift this script exists to catch.
#
# SYS_CHROOT belongs to the sshd sidecar, not the guest container (sshd
# privilege-separates by chrooting its unprivileged listener). This one container
# plays both roles, so it gets the union — the pod keeps them apart.
log "starting $IMAGE"
CID="$(docker run -d --rm \
  --cap-drop ALL \
  --cap-add CHOWN --cap-add FOWNER --cap-add DAC_OVERRIDE \
  --cap-add SETUID --cap-add SETGID --cap-add SYS_CHROOT \
  --security-opt no-new-privileges \
  --tmpfs /run/proteos:rw,mode=0700 \
  -v "$VOLUME:/persist" \
  -p 127.0.0.1:11024:1024 \
  -p 127.0.0.1:11025:1025 \
  -p 127.0.0.1:11026:1026 \
  "$IMAGE")" || die "docker run failed"
log "container $CID"

# --- 1. readiness -----------------------------------------------------------
log "waiting for GET /healthz on 1024"
for i in $(seq 1 60); do
  if curl -fsS --max-time 2 http://127.0.0.1:11024/healthz >/dev/null 2>&1; then break; fi
  if ! docker ps -q --no-trunc | grep -q "$CID"; then
    docker logs "$CID" 2>&1 | tail -30
    die "container exited before becoming healthy"
  fi
  sleep 1
  [[ $i -eq 60 ]] && { docker logs "$CID" 2>&1 | tail -30; die "guest agent never became healthy"; }
done
ok "guest agent healthy on 1024"

# --- 2. /info: persistence is real, not degraded ----------------------------
INFO="$(curl -fsS --max-time 5 http://127.0.0.1:11024/info)" || die "GET /info failed"
echo "$INFO" | grep -q '"persist":"dir"' \
  || die "expected persist=dir (guestwire.PersistDir), got: $INFO"
ok "persist is in dir mode (no /dev/vdb, no mount): $INFO"

# --- 3. a terminal session actually starts ----------------------------------
log "opening a terminal WebSocket"
python3 - <<'PY' || die "terminal WebSocket check failed"
import base64, os, socket, sys

s = socket.create_connection(("127.0.0.1", 11024), timeout=15)
key = base64.b64encode(os.urandom(16)).decode()
s.sendall((
    "GET /terminal?session=verify HTTP/1.1\r\n"
    "Host: 127.0.0.1\r\n"
    "Upgrade: websocket\r\n"
    "Connection: Upgrade\r\n"
    f"Sec-WebSocket-Key: {key}\r\n"
    "Sec-WebSocket-Version: 13\r\n\r\n"
).encode())

buf = b""
while b"\r\n\r\n" not in buf:
    chunk = s.recv(4096)
    if not chunk:
        sys.exit("connection closed during handshake")
    buf += chunk
head, _, rest = buf.partition(b"\r\n\r\n")
if b"101" not in head.split(b"\r\n")[0]:
    sys.exit("no 101 upgrade: " + head.decode(errors="replace"))

# PTY input is carried in BINARY frames; TEXT frames are JSON control messages
# (resize), so an opcode mix-up here would silently do nothing.
def mask(payload, opcode=0x2):
    m = os.urandom(4)
    masked = bytes(b ^ m[i % 4] for i, b in enumerate(payload))
    return bytes([0x80 | opcode, 0x80 | len(payload)]) + m + masked

# Ask the shell to print a token, then read frames until it comes back.
s.sendall(mask(b'echo proteos-terminal-ok\n'))
s.settimeout(15)
seen = rest
deadline_frames = 200
while b"proteos-terminal-ok" not in seen and deadline_frames:
    deadline_frames -= 1
    try:
        chunk = s.recv(65536)
    except socket.timeout:
        break
    if not chunk:
        break
    seen += chunk
if b"proteos-terminal-ok" not in seen:
    sys.exit("terminal never echoed the probe token; got %r" % seen[-400:])
print("terminal session round-tripped")
PY
ok "terminal WebSocket upgraded and a PTY session ran a command"

# --- 4. the port-preview forward --------------------------------------------
# Start a trivial backend inside the container on 8080, then reach it from the
# host through the preview forwarder on 1026 using the node-agent's one-line
# port preamble (agentapi.PreviewPreamble).
log "checking the port-preview forward (1026 → 127.0.0.1:8080)"
docker exec -d "$CID" /usr/local/bin/node -e \
  "require('http').createServer((q,s)=>s.end('proteos-preview-ok')).listen(8080,'127.0.0.1')" \
  || die "could not start the in-container preview backend"
for _ in $(seq 1 20); do
  docker exec "$CID" sh -c 'exec 3<>/dev/tcp/127.0.0.1/8080' 2>/dev/null && break
  sleep 0.5
done
python3 - <<'PY' || die "preview forward check failed"
import socket, sys
s = socket.create_connection(("127.0.0.1", 11026), timeout=15)
s.sendall(b"8080\n")                      # agentapi.PreviewPreamble(8080)
s.sendall(b"GET / HTTP/1.0\r\nHost: preview\r\n\r\n")
data = b""
s.settimeout(10)
while True:
    try:
        chunk = s.recv(4096)
    except socket.timeout:
        break
    if not chunk:
        break
    data += chunk
if b"proteos-preview-ok" not in data:
    sys.exit("preview did not reach the backend; got %r" % data[-400:])
print("preview forward reached 127.0.0.1:8080")
PY
ok "port-preview forward bridges to an in-container loopback port"

# --- 5. sshd, the way the pod runs it ---------------------------------------
# In a machine pod sshd is a SECOND CONTAINER from this same image, sharing the
# pod's network namespace. There is no pod here, so the equivalent is to run the
# same entrypoint inside this container: same image, same loopback, same volume.
# What is actually under test is the configuration — that the host keys land on
# the persist volume rather than in the image, and that sshd and the guest agent
# agree on which directory is the session user's home. They disagree easily: sshd
# reads $HOME and AuthorizedKeysFile from the passwd entry, while the guest agent
# uses the persisted home, and a mismatch means ssh authenticates against a file
# nothing ever writes.
log "starting sshd the way the pod's sidecar does"
docker exec -d "$CID" /usr/local/bin/proteos-sshd || die "could not start proteos-sshd"
# bash, not sh: /dev/tcp is a bash feature and the image's /bin/sh is dash, where
# this probe fails no matter what sshd is doing.
for i in $(seq 1 30); do
  docker exec "$CID" bash -lc 'exec 3<>/dev/tcp/127.0.0.1/22' >/dev/null 2>&1 && break
  sleep 1
  [[ $i -eq 30 ]] && { docker logs "$CID" 2>&1 | tail -20; die "sshd never listened on 127.0.0.1:22"; }
done
ok "sshd listening on 127.0.0.1:22"

docker exec "$CID" sh -lc 'test -s /persist/ssh_host_keys/ssh_host_ed25519_key'   || die "host keys are not on the persist volume; a machine would change identity on every restart"
docker exec "$CID" sh -lc 'test -z "$(ls -A /etc/ssh/ssh_host_* 2>/dev/null)"'   || die "the IMAGE ships host keys; every machine built from it would share one identity"
ok "host keys are per-machine, on the persist volume"

HOME_PASSWD="$(docker exec "$CID" sh -lc 'getent passwd dev | cut -d: -f6')"
[[ $HOME_PASSWD == /persist/* ]]   || die "dev's passwd home is $HOME_PASSWD, not on the persist volume — ssh sessions would land in an ephemeral home"
ok "dev's home is the persisted one ($HOME_PASSWD)"

log "logging in over ssh with a key where the injector puts it"
docker exec "$CID" sh -lc '
  set -e
  ssh-keygen -q -t ed25519 -f /tmp/verify_key -N ""
  install -d -m 700 -o dev -g dev "'"$HOME_PASSWD"'/.ssh"
  install -m 600 -o dev -g dev /tmp/verify_key.pub "'"$HOME_PASSWD"'/.ssh/authorized_keys"
' || die "could not install a test authorized_keys"
SSH_OUT="$(docker exec "$CID" sh -lc   'ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes        -i /tmp/verify_key dev@127.0.0.1 "id -un; pwd" 2>&1')"   || die "ssh login failed: $SSH_OUT"
echo "$SSH_OUT" | grep -q '^dev$' || die "ssh session is not the dev user: $SSH_OUT"
echo "$SSH_OUT" | grep -q "^$HOME_PASSWD\$" || die "ssh session did not start in the persisted home: $SSH_OUT"
ok "ssh key auth works and lands in the persisted home"

# --- 6. the baked platform layer --------------------------------------------
log "checking the platform layer"
check() { # check DESCRIPTION COMMAND...
  local desc="$1"; shift
  local out
  out="$(docker exec "$CID" sh -lc "$*" 2>&1)" || die "$desc failed: $out"
  ok "$desc: $(echo "$out" | head -1)"
}
check "claude"       "claude --version"
check "git"          "git --version"
check "code-server"  "code-server --version | head -1"
check "node"         "node --version"
check "dev user"     "id dev"
check "gh wrapper"   "test -x /usr/local/bin/gh && test -x /usr/local/libexec/proteos/gh && echo 'gh wrapper + libexec binary present'"
check "providers snippet" "test -r /etc/profile.d/proteos-providers.sh && echo present"
check "no sudo"      "! command -v sudo >/dev/null && echo 'sudo absent (pod boundary is thinner than a microVM)'"

# --- 7. provenance ----------------------------------------------------------
REL="$(docker exec "$CID" cat /etc/proteos-release)" || die "no /etc/proteos-release"
echo "$REL" | grep -q '^PROTEOS_BACKEND=kubernetes' || die "release stamp missing PROTEOS_BACKEND"
echo "$REL" | grep -q '^PROTEOS_CLAUDE_SHA256=[0-9a-f]\{64\}$' || die "release stamp missing a pinned Claude digest"
ok "/etc/proteos-release stamped:"
echo "$REL" | sed 's/^/       /'

printf '\n\e[1;32m[verify-guest] all checks passed for %s\e[0m\n' "$IMAGE"
