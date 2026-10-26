# syntax=docker/dockerfile:1
#
# guest.Dockerfile — the ProteOS guest image for the Kubernetes backend (K0).
#
# This is the OCI sibling of image/build-rootfs.sh: the same platform layer (git,
# vim, gh + auth wrapper, code-server, Claude Code, Node + provider CLIs, the
# `dev` user, the providers profile.d snippet, /etc/proteos-release), assembled
# for a pod instead of a Firecracker microVM. It carries no kernel, no systemd
# and no /dev/vdb: the guest-agent is PID 1 and listens on TCP rather than vsock,
# which is exactly how the dev driver already runs it (guestagent's listen
# package has supported tcp: since Phase 3, and persist has a dir mode that needs
# no mount).
#
# The pinned-artifact fetches (Claude/Node/code-server/gh: resolve, download,
# verify sha256) come from image/lib/fetch.sh, which build-rootfs.sh sources too
# — the two images install the same versions, verified the same way.
#
# Build context is the REPO ROOT (it needs guestagent/ and image/):
#
#   docker build -f image/guest.Dockerfile --build-arg TEMPLATE=base \
#     -t ghcr.io/ipedrazas/proteos-guest:base-dev .
#
# TEMPLATE selects the language layer, matching image/bake-templates.sh:
#   base | go | node | python | full
#
# WHAT IS DELIBERATELY ABSENT vs the ext4 image:
#   * systemd — the guest-agent is PID 1 (it already handles SIGTERM with a 5s
#     grace period). sshd therefore has no supervisor here; the Kubernetes
#     backend runs it as a second container in the pod instead, from THIS image
#     with /usr/local/bin/proteos-sshd as its command (K4).
#   * NOPASSWD sudo for the `dev` user. On Firecracker the microVM boundary makes
#     in-guest root uninteresting; a pod shares the host kernel, so the sudoers
#     drop-in is not carried over. sudo is not installed at all.
#   * The LUKS volume: encryption at rest is the StorageClass's job here. See
#     SECURITY.md for the isolation delta.

ARG TEMPLATE=base
ARG GO_IMAGE=golang:1.27
ARG BASE_IMAGE=ubuntu:24.04

# --- 1. the static guest agent ----------------------------------------------
# Same build as build-rootfs.sh's step 1: CGO off, -trimpath, version stamped.
# GOWORK=off because only the guestagent module is in the context (it imports
# nothing from the other modules — Phase 3 decision #2).
FROM ${GO_IMAGE} AS guestagent-build
WORKDIR /src
# GOTOOLCHAIN=auto: the golang:1.27 tag can trail guestagent/go.mod's exact
# patch requirement, and the builder has network to fetch the pinned toolchain.
ENV CGO_ENABLED=0 GOOS=linux GOWORK=off GOTOOLCHAIN=auto
ARG GUESTAGENT_VERSION=ga-dev
COPY guestagent/ ./guestagent/
RUN cd guestagent \
    && go build -trimpath -ldflags "-s -w -X main.version=${GUESTAGENT_VERSION}" \
       -o /out/guestagent ./cmd/guestagent

# --- 2. pinned third-party artifacts ----------------------------------------
# Fetched in their own stage so a language-layer rebuild does not re-download
# ~400 MiB, and so a checksum failure aborts before anything is installed.
FROM ${BASE_IMAGE} AS artifacts
SHELL ["/bin/bash", "-o", "pipefail", "-c"]
RUN apt-get update -qq \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
       ca-certificates curl jq xz-utils \
    && rm -rf /var/lib/apt/lists/*

COPY image/lib/fetch.sh /opt/proteos/lib/fetch.sh

# Version pins. Empty ⇒ resolve upstream's current release (what build-rootfs.sh
# does by default); pass an explicit value to reproduce an older image.
ARG CLAUDE_VERSION=stable
ARG NODE_VERSION=
ARG NODE_SHA256=
ARG CODESERVER_VERSION=
ARG CODESERVER_SHA256=
ARG GH_VERSION=
ARG GH_SHA256=

# One layer, one provenance record: every artifact's resolved version + digest is
# written to /opt/proteos/artifacts.env, which the final stage folds into
# /etc/proteos-release.
RUN set -euo pipefail \
    && source /opt/proteos/lib/fetch.sh \
    && mkdir -p /opt/proteos/dl \
    && fetch_claude "${CLAUDE_VERSION}" /opt/proteos/dl/claude \
    && fetch_node "${NODE_VERSION}" "${NODE_SHA256}" /opt/proteos/dl/node.tar.xz \
    && fetch_codeserver "${CODESERVER_VERSION}" "${CODESERVER_SHA256}" /opt/proteos/dl/code-server.tar.gz \
    && fetch_gh "${GH_VERSION}" "${GH_SHA256}" /opt/proteos/dl/gh.tar.gz \
    && { echo "PROTEOS_CLAUDE_VERSION=$CLAUDE_VERSION"; \
         echo "PROTEOS_CLAUDE_SHA256=$CLAUDE_SHA256"; \
         echo "PROTEOS_NODE_VERSION=$NODE_VERSION"; \
         echo "PROTEOS_NODE_SHA256=$NODE_SHA256"; \
         echo "PROTEOS_CODESERVER_VERSION=$CS_VERSION"; \
         echo "PROTEOS_CODESERVER_SHA256=$CS_SHA256"; \
         echo "PROTEOS_GH_VERSION=$GH_VERSION"; \
         echo "PROTEOS_GH_SHA256=$GH_SHA256"; \
       } >/opt/proteos/artifacts.env

# --- 3. the platform layer --------------------------------------------------
# Identical in intent to build-rootfs.sh's step 3, minus the extract-only
# dpkg-deb dance: that exists because reinstalling the base closure inside a
# loop-mounted Firecracker rootfs breaks snapshot resume. A container layer has
# no snapshot to break, so plain apt is both correct and simpler.
FROM ${BASE_IMAGE} AS platform
SHELL ["/bin/bash", "-o", "pipefail", "-c"]
ENV DEBIAN_FRONTEND=noninteractive LANG=C.UTF-8

# openssh-server is here for the pod's sshd sidecar (K4). Its postinst generates
# host keys AT BUILD TIME; baked into the image they would be identical on every
# machine started from it, so any machine could impersonate any other to a client
# that pinned the fingerprint. They are deleted in the same layer that creates
# them — proteos-sshd generates a per-machine set on the machine's own volume at
# first start.
RUN apt-get update -qq \
    && apt-get install -y --no-install-recommends \
       ca-certificates curl git vim less unzip xz-utils procps tini locales \
       openssh-client openssh-server \
    && rm -rf /var/lib/apt/lists/* \
    && rm -f /etc/ssh/ssh_host_*

# The `dev` user PTY sessions drop to (Phase 8). The guest-agent resolves it by
# NAME, so the uid is free — but pin it to 1000 anyway so files on the PVC keep
# stable ownership across image rebuilds. ubuntu:24.04 ships its own uid-1000
# `ubuntu` user; remove it first. No sudoers drop-in (see the header).
#
# Its home is the PERSISTED home, not the image's /home/dev. Three things
# resolve "~" for this user and they have to mean the same directory: the guest
# agent starts sessions in the persisted home (dir mode puts it at
# <persist>/home), the injector writes profile files and .ssh/authorized_keys
# there, and sshd reads $HOME and AuthorizedKeysFile from THIS passwd field.
# Leave it at /home/dev and an ssh session lands in a directory that dies with
# the container, holding none of the user's files and none of their keys. The
# ext4 image gets the same alignment by bind-mounting the persisted home over
# /home/dev — a mount namespace this backend does not have.
RUN userdel -r ubuntu 2>/dev/null || true; \
    groupadd -g 1000 dev \
    && useradd -m -u 1000 -g 1000 -s /bin/bash dev \
    && usermod -d /persist/home dev

# Node + npm at /usr/local (the runtime for the npm-distributed provider CLIs).
COPY --from=artifacts /opt/proteos/dl/node.tar.xz /tmp/node.tar.xz
RUN tar -xJf /tmp/node.tar.xz -C /usr/local --strip-components=1 && rm /tmp/node.tar.xz

# code-server: the standalone release bundles its own Node and binds loopback
# only; the guest-agent's web forward starts it lazily (Phase 8 decision #5).
COPY --from=artifacts /opt/proteos/dl/code-server.tar.gz /tmp/code-server.tar.gz
RUN mkdir -p /usr/local/lib/code-server \
    && tar -xzf /tmp/code-server.tar.gz -C /usr/local/lib/code-server --strip-components=1 \
    && ln -sf ../lib/code-server/bin/code-server /usr/local/bin/code-server \
    && rm /tmp/code-server.tar.gz

# gh behind the ProteOS auth wrapper (Phase 7): the real binary lives in libexec
# and /usr/local/bin/gh is the wrapper that resolves the injected token.
COPY --from=artifacts /opt/proteos/dl/gh.tar.gz /tmp/gh.tar.gz
COPY image/gh-wrapper.sh /usr/local/bin/gh
RUN mkdir -p /tmp/gh /usr/local/libexec/proteos \
    && tar -xzf /tmp/gh.tar.gz -C /tmp/gh --strip-components=1 \
    && install -m 0755 /tmp/gh/bin/gh /usr/local/libexec/proteos/gh \
    && chmod 0755 /usr/local/bin/gh \
    && rm -rf /tmp/gh /tmp/gh.tar.gz

# Claude Code, pinned + digest-verified in the artifacts stage.
COPY --from=artifacts /opt/proteos/dl/claude /usr/local/bin/claude
RUN chmod 0755 /usr/local/bin/claude

# Managed settings (autoupdater off — the image pins the version) and the
# providers snippet that sources /run/proteos/env/*.env into login shells
# (Phase 5 decision #9a). Sessions run `<shell> -l`, so /etc/profile.d applies.
COPY image/claude-managed-settings.json /etc/claude-code/managed-settings.json
COPY image/profile.d-proteos-providers.sh /etc/profile.d/proteos-providers.sh
RUN chmod 0644 /etc/claude-code/managed-settings.json /etc/profile.d/proteos-providers.sh

# sshd's config and its sidecar entrypoint. sshd is NOT started here: it runs as
# a second container in the machine pod, sharing this image and the pod's
# network namespace, so the guest agent's ssh forward reaches it on 127.0.0.1:22
# exactly as it does inside a microVM (K4).
#
# The sed is not cosmetic: pam_loginuid ties a session to an audit login id by
# writing /proc/self/loginuid, which needs CAP_AUDIT_CONTROL and an audit
# subsystem the container is not part of. Left `required`, every ssh session
# fails at the session stage — "Cannot make/remove an entry for the specified
# session" — however well authentication went. Optional keeps the module's
# bookkeeping without letting its absence deny a login.
COPY image/sshd_config.k8s /etc/ssh/sshd_config
COPY image/proteos-sshd.sh /usr/local/bin/proteos-sshd
RUN chmod 0644 /etc/ssh/sshd_config \
    && chmod 0755 /usr/local/bin/proteos-sshd \
    && sed -i 's/^\(session\s*\)required\(\s*pam_loginuid\.so\)/\1optional\2/' /etc/pam.d/sshd

# The guest agent. The path matters: it is also the credential helper git
# invokes (Phase 7 decision #5 — the same static binary, dispatched by argv[1]),
# and guestwire.HelperBinPath hardcodes "/usr/local/bin/guestagent
# git-credential" into every pushed ~/.gitconfig.
COPY --from=guestagent-build /out/guestagent /usr/local/bin/guestagent

# /run/proteos is where the control plane's injected secrets land. In a pod it
# is a memory-backed emptyDir (the manifests mount it); create the mount point
# so an unmounted run still has somewhere to write.
RUN mkdir -p /run/proteos/env && chmod 0700 /run/proteos/env

# --- 4. language layers -----------------------------------------------------
# One stage per template id, mirroring image/bake-templates.sh's lang_flags.

FROM platform AS lang-base

# The Go toolchain is fetched in its own stage so only the templates that use it
# pay the download. Same fetch_go as build-rootfs.sh — including the reason it
# does NOT use a detached <tarball>.sha256 (go.dev answers that path with its
# HTML shell and a 200).
FROM artifacts AS go-artifact
ARG GO_VERSION=1.27.3
ARG GO_SHA256=
RUN set -euo pipefail \
    && source /opt/proteos/lib/fetch.sh \
    && fetch_go "${GO_VERSION}" "${GO_SHA256}" /opt/proteos/dl/go.tar.gz

FROM platform AS lang-go
COPY --from=go-artifact /opt/proteos/dl/go.tar.gz /tmp/go.tar.gz
RUN tar -xzf /tmp/go.tar.gz -C /usr/local && rm /tmp/go.tar.gz
ENV PATH=/usr/local/go/bin:$PATH

FROM platform AS lang-node
# Node is already in the platform layer (it is the provider CLIs' runtime); this
# template just declares it as the machine's language toolchain.

FROM platform AS lang-python
RUN apt-get update -qq \
    && apt-get install -y --no-install-recommends \
       python3 python3-pip python3-venv python3-dev build-essential \
    && rm -rf /var/lib/apt/lists/*

FROM lang-go AS lang-full
RUN apt-get update -qq \
    && apt-get install -y --no-install-recommends \
       python3 python3-pip python3-venv python3-dev build-essential \
    && rm -rf /var/lib/apt/lists/*
ENV RUSTUP_HOME=/usr/local/rustup CARGO_HOME=/usr/local/cargo
RUN curl -fsSL https://sh.rustup.rs | sh -s -- -y --no-modify-path --profile minimal \
    && chmod -R a+rX "$RUSTUP_HOME" "$CARGO_HOME"
ENV PATH=/usr/local/cargo/bin:$PATH

# --- 5. the guest image -----------------------------------------------------
FROM lang-${TEMPLATE} AS guest
ARG TEMPLATE
ARG GUESTAGENT_VERSION=ga-dev

# Provenance the guest (and humans) can read, same file the ext4 image writes.
COPY --from=artifacts /opt/proteos/artifacts.env /tmp/artifacts.env
RUN { echo "PROTEOS_ROOTFS_BASE=oci-ubuntu-24.04"; \
      echo "PROTEOS_BACKEND=kubernetes"; \
      echo "PROTEOS_TEMPLATE=${TEMPLATE}"; \
      echo "PROTEOS_GUESTAGENT_VERSION=${GUESTAGENT_VERSION}"; \
      echo "PROTEOS_GUESTAGENT_FEATURES=terminal,persist,providers,claude,git,user,codeserver,node,preview"; \
      echo "PROTEOS_GIT_VERSION=$(git --version | awk '{print $3}')"; \
      cat /tmp/artifacts.env; \
    } >/etc/proteos-release \
    && rm /tmp/artifacts.env && chmod 0644 /etc/proteos-release

# The listener set the Kubernetes driver dials on the pod IP: terminal 1024,
# code-server 1025, port-preview 1026, inbound ssh 1027 (agentapi's Guest*Port).
# The ssh forward's backend is 127.0.0.1:22 — the sshd sidecar, reachable
# because pod containers share a network namespace.
ENV PROTEOS_GUEST_LISTEN=tcp:0.0.0.0:1024 \
    PROTEOS_GUEST_WEB_LISTEN=tcp:0.0.0.0:1025 \
    PROTEOS_GUEST_PREVIEW_LISTEN=tcp:0.0.0.0:1026 \
    PROTEOS_GUEST_SSH_LISTEN=tcp:0.0.0.0:1027 \
    PROTEOS_GUEST_PERSIST=/persist \
    PROTEOS_GUEST_ENV_DIR=/run/proteos/env \
    PROTEOS_GUEST_RUN_AS_USER=dev \
    PROTEOS_CODESERVER_BIN=/usr/local/bin/code-server
EXPOSE 1024 1025 1026 1027

# The guest-agent is PID 1 but spawns PTYs and code-server, so it needs a reaper
# for orphaned grandchildren; tini is that (systemd's only remaining job here).
# The agent still gets SIGTERM directly and runs its own 5s graceful shutdown.
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/guestagent"]
