# shellcheck shell=bash
# shellcheck disable=SC2034  # the <THING>_VERSION/_SHA256 assignments are this
# file's outputs: the sourcing build script reads them into its manifest.
# image/lib/fetch.sh — the pinned-artifact fetch layer shared by both guest image
# builds: the Firecracker ext4 bake (image/build-rootfs.sh, which loop-mounts and
# chroots) and the OCI build (image/guest.Dockerfile, which just installs into
# the layer). Everything here is host-side: resolve a version, download to a
# path, verify a sha256, export <THING>_VERSION / <THING>_SHA256. Nothing here
# knows about a mount point, a chroot, or a container.
#
# That split is the whole point: unpacking differs between the two backends
# (`sudo tar -C $mnt` vs `tar -C /usr/local`), but *what* we install and *how we
# prove it is what we asked for* must not drift (K-plan risk #6). Both builds
# source this file; both record the same versions and digests in their manifest.
#
# Usage: source it after defining log/ok/die (it supplies fallbacks if not).
#
#   source "$(dirname "$0")/lib/fetch.sh"
#   fetch_claude stable /tmp/claude       # ⇒ CLAUDE_VERSION, CLAUDE_SHA256
#   fetch_node "" "" /tmp/node.tar.xz     # ⇒ NODE_VERSION, NODE_SHA256
#   fetch_codeserver "" "" /tmp/cs.tar.gz # ⇒ CS_VERSION, CS_SHA256
#   fetch_gh "" "" /tmp/gh.tar.gz         # ⇒ GH_VERSION, GH_SHA256

# Fallback logging for callers that do not define their own (the Dockerfile
# stage runs this with `sh -c bash`, not under build-rootfs.sh).
if ! declare -F log >/dev/null 2>&1; then
  log() { printf '[fetch] %s\n' "$*"; }
fi
if ! declare -F ok >/dev/null 2>&1; then
  ok() { printf '[ ok ] %s\n' "$*"; }
fi
if ! declare -F die >/dev/null 2>&1; then
  die() {
    printf '[fail] %s\n' "$*" >&2
    exit 1
  }
fi

# dl URL [OUTFILE] — fetch over curl or wget (whichever is present).
dl() {
  if command -v curl >/dev/null 2>&1; then
    if [[ -n ${2:-} ]]; then curl -fsSL -o "$2" "$1"; else curl -fsSL "$1"; fi
  elif command -v wget >/dev/null 2>&1; then
    if [[ -n ${2:-} ]]; then wget -qO "$2" "$1"; else wget -qO - "$1"; fi
  else
    die "need curl or wget to fetch pinned artifacts"
  fi
}

# sha256_of FILE — the file's sha256 hex digest (sha256sum or shasum -a 256).
sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

# --- arch maps --------------------------------------------------------------
# Each upstream names the same two architectures differently; keep the maps
# beside the fetchers that use them.

node_arch() {
  case "$(uname -m)" in
    x86_64 | amd64) echo "x64" ;;
    arm64 | aarch64) echo "arm64" ;;
    *) die "unsupported arch for Node: $(uname -m)" ;;
  esac
}

cs_arch() {
  case "$(uname -m)" in
    x86_64 | amd64) echo "amd64" ;;
    arm64 | aarch64) echo "arm64" ;;
    *) die "unsupported arch for code-server: $(uname -m)" ;;
  esac
}

# go_arch is the go.dev dist tag; gh's release tarballs use the same names.
go_arch() {
  case "$(uname -m)" in
    x86_64 | amd64) echo "amd64" ;;
    arm64 | aarch64) echo "arm64" ;;
    *) die "unsupported arch for Go: $(uname -m)" ;;
  esac
}

# --- Claude Code ------------------------------------------------------------

# fetch_claude TARGET DEST — download the pinned Claude Code native binary from
# Anthropic's release endpoint and verify it against the published manifest.json
# checksum (the same artifact + integrity check the upstream bootstrap.sh uses).
# TARGET is "stable", "latest", or an exact X.Y.Z. Sets CLAUDE_VERSION and
# CLAUDE_SHA256; leaves DEST executable.
fetch_claude() {
  local target="$1" dest="$2"
  local base="https://downloads.claude.ai/claude-code-releases"

  local arch platform
  case "$(uname -m)" in
    x86_64 | amd64) arch="x64" ;;
    arm64 | aarch64) arch="arm64" ;;
    *) die "unsupported arch for Claude Code: $(uname -m)" ;;
  esac
  platform="${CLAUDE_PLATFORM:-linux-$arch}" # base is glibc; set CLAUDE_PLATFORM for musl

  # Resolve a channel to a concrete version; accept an explicit X.Y.Z as-is.
  local version
  if [[ "$target" =~ ^[0-9]+\.[0-9]+\.[0-9]+ ]]; then
    version="$target"
  else
    version="$(dl "$base/$target")" || die "resolve Claude channel '$target'"
  fi
  [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+ ]] || die "bad Claude version resolved from '$target': $version"

  local manifest checksum
  manifest="$(dl "$base/$version/manifest.json")" || die "fetch Claude manifest for $version"
  if command -v jq >/dev/null 2>&1; then
    checksum="$(printf '%s' "$manifest" | jq -r ".platforms[\"$platform\"].checksum // empty")"
  else
    # bash fallback: collapse whitespace, then regex the platform's checksum.
    checksum="$(printf '%s' "$manifest" | tr -d '\n\r\t' \
      | grep -oE "\"$platform\"[^}]*\"checksum\"[[:space:]]*:[[:space:]]*\"[a-f0-9]{64}\"" \
      | grep -oE '[a-f0-9]{64}' | head -1)"
  fi
  [[ "$checksum" =~ ^[a-f0-9]{64}$ ]] || die "no checksum for platform '$platform' in Claude manifest $version"

  dl "$base/$version/$platform/claude" "$dest" || die "download Claude binary $version/$platform"
  local actual
  actual="$(sha256_of "$dest")"
  [[ "$actual" == "$checksum" ]] || die "Claude checksum mismatch: manifest $checksum, got $actual"
  chmod +x "$dest"
  CLAUDE_VERSION="$version"
  CLAUDE_SHA256="$checksum"
  ok "fetched Claude Code $version ($platform), sha256 verified against manifest"
}

# --- Node -------------------------------------------------------------------

# fetch_node VERSION SHA256 DEST — download the Node tarball from nodejs.org and
# verify its sha256 (against the published SHASUMS256.txt when SHA256 is empty).
# An empty VERSION resolves the latest LTS. Sets NODE_VERSION/NODE_SHA256.
fetch_node() {
  local version="$1" want_sha="$2" dest="$3"
  local arch tarball url
  arch="$(node_arch)"

  # Resolve "latest LTS" from the canonical dist index.json when unpinned. It is
  # sorted newest-first, so the first entry whose "lts" is not false is the latest
  # LTS release.
  if [[ -z $version ]]; then
    log "resolving latest Node LTS"
    local index
    index="$(dl "https://nodejs.org/dist/index.json")" || die "fetch Node index.json"
    if command -v jq >/dev/null 2>&1; then
      version="$(printf '%s' "$index" | jq -r 'map(select(.lts != false))[0].version')"
    else
      # One object per line (objects start with '{'; nested arrays use '['), then
      # take the first with an "lts":"<name>" (false has no quote after the colon).
      version="$(printf '%s' "$index" | tr '{' '\n' \
        | grep -m1 '"lts":"' \
        | grep -oE '"version":"v[0-9]+\.[0-9]+\.[0-9]+"' \
        | grep -oE 'v[0-9.]+')"
    fi
    [[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "could not resolve latest Node LTS (got '$version')"
    ok "latest Node LTS is $version"
  fi
  tarball="node-${version}-linux-${arch}.tar.xz"
  url="https://nodejs.org/dist/${version}/${tarball}"
  NODE_VERSION="$version"

  log "fetching Node ${version} (${arch})"
  dl "$url" "$dest" || die "download Node $version"

  local actual
  actual="$(sha256_of "$dest")"
  if [[ -z $want_sha ]]; then
    # Verify against the dist SHASUMS256.txt rather than trusting blindly.
    want_sha="$(dl "https://nodejs.org/dist/${version}/SHASUMS256.txt" | grep " ${tarball}\$" | awk '{print $1}')"
    [[ -n $want_sha ]] || die "no SHASUMS256 entry for $tarball"
  fi
  [[ "$actual" == "$want_sha" ]] || die "Node sha256 mismatch: expected $want_sha, got $actual"
  NODE_SHA256="$want_sha"
  ok "Node tarball sha256 verified ($actual)"
}

# --- code-server ------------------------------------------------------------

# fetch_codeserver VERSION SHA256 DEST — download the code-server release tarball
# and verify its sha256 when one is given (upstream publishes no checksums file,
# so an unpinned build records the tarball's own digest and warns). An empty
# VERSION resolves the latest release. Sets CS_VERSION/CS_SHA256.
fetch_codeserver() {
  local version="$1" want_sha="$2" dest="$3"
  local arch tarball url
  arch="$(cs_arch)"

  if [[ -z $version ]]; then
    log "resolving latest code-server release"
    local tag
    tag="$(dl https://api.github.com/repos/coder/code-server/releases/latest \
      | grep -oE '"tag_name"[[:space:]]*:[[:space:]]*"v[0-9]+\.[0-9]+\.[0-9]+"' \
      | grep -oE 'v[0-9]+\.[0-9]+\.[0-9]+' | head -1)" || die "resolve code-server latest"
    [[ -n $tag ]] || die "could not resolve latest code-server release"
    version="${tag#v}"
    ok "latest code-server is $version"
  fi
  [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+ ]] || die "bad code-server version: $version"

  tarball="code-server-${version}-linux-${arch}.tar.gz"
  url="https://github.com/coder/code-server/releases/download/v${version}/${tarball}"
  log "fetching code-server ${version} (${arch})"
  dl "$url" "$dest" || die "download code-server $version"

  local actual
  actual="$(sha256_of "$dest")"
  if [[ -n $want_sha ]]; then
    [[ "$actual" == "$want_sha" ]] || die "code-server sha256 mismatch: expected $want_sha, got $actual"
    ok "code-server sha256 verified ($actual)"
  else
    log "WARNING: no code-server sha256 given; pinning to the tarball's own sha256 ($actual)"
  fi
  CS_SHA256="$actual"
  CS_VERSION="$version"
}

# --- gh (GitHub CLI) --------------------------------------------------------

# fetch_gh VERSION SHA256 DEST — download the gh release tarball and verify its
# sha256 (against the release's checksums.txt when SHA256 is empty). An empty
# VERSION resolves the latest release. Sets GH_VERSION/GH_SHA256.
fetch_gh() {
  local version="$1" want_sha="$2" dest="$3"
  local arch tarball url
  arch="$(go_arch)" # gh uses the same amd64/arm64 release tags as Go

  if [[ -z $version ]]; then
    log "resolving latest gh (GitHub CLI) release"
    local tag
    tag="$(dl https://api.github.com/repos/cli/cli/releases/latest \
      | grep -oE '"tag_name"[[:space:]]*:[[:space:]]*"v[0-9]+\.[0-9]+\.[0-9]+"' \
      | grep -oE 'v[0-9]+\.[0-9]+\.[0-9]+' | head -1)" || die "resolve gh latest"
    [[ -n $tag ]] || die "could not resolve latest gh release"
    version="${tag#v}"
    ok "latest gh is $version"
  fi
  version="${version#v}" # accept a pin given with or without the leading v
  [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+ ]] || die "bad gh version: $version"

  tarball="gh_${version}_linux_${arch}.tar.gz"
  url="https://github.com/cli/cli/releases/download/v${version}/${tarball}"
  log "fetching gh ${version} (${arch})"
  dl "$url" "$dest" || die "download gh $version"

  local actual
  actual="$(sha256_of "$dest")"
  if [[ -z $want_sha ]]; then
    want_sha="$(dl "https://github.com/cli/cli/releases/download/v${version}/gh_${version}_checksums.txt" 2>/dev/null \
      | grep " ${tarball}\$" | awk '{print $1}')" || true
  fi
  if [[ -n $want_sha ]]; then
    [[ "$actual" == "$want_sha" ]] || die "gh sha256 mismatch: expected $want_sha, got $actual"
    ok "gh tarball sha256 verified ($actual)"
  else
    log "WARNING: could not fetch gh checksums; pinning to the tarball's own sha256 ($actual)"
  fi
  GH_SHA256="$actual"
  GH_VERSION="$version"
}

# --- Go ---------------------------------------------------------------------

# fetch_go VERSION SHA256 DEST — download the Go toolchain tarball from go.dev and
# verify its sha256. When SHA256 is empty the digest comes from go.dev's JSON
# index, NOT from a detached <tarball>.sha256: that path returns the site's HTML
# shell with HTTP 200, so `curl -f` does not catch it and the page gets mistaken
# for a checksum. Sets GO_VERSION/GO_SHA256.
fetch_go() {
  local version="$1" want_sha="$2" dest="$3"
  local arch tarball url
  arch="$(go_arch)"
  tarball="go${version}.linux-${arch}.tar.gz"
  url="https://go.dev/dl/${tarball}"
  log "fetching Go ${version} (${arch})"
  dl "$url" "$dest" || die "download Go $version"

  local actual
  actual="$(sha256_of "$dest")"
  if [[ -z $want_sha ]]; then
    want_sha="$(dl "https://go.dev/dl/?mode=json&include=all" 2>/dev/null | tr -d '\n ' \
      | grep -oE "\"filename\":\"${tarball//./\\.}\"[^}]*\"sha256\":\"[0-9a-f]{64}\"" \
      | grep -oE '[0-9a-f]{64}' | head -1)" || true
  fi
  if [[ -n $want_sha ]]; then
    [[ "$actual" == "$want_sha" ]] || die "Go sha256 mismatch: expected $want_sha, got $actual"
    ok "Go tarball sha256 verified ($actual)"
  else
    log "WARNING: could not fetch Go checksum; pinning to the tarball's own sha256 ($actual)"
  fi
  GO_SHA256="$actual"
  GO_VERSION="$version"
}
