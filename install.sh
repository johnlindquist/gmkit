#!/usr/bin/env bash
# gmkit installer — installs gmcli and gmtui globally.
#
#   One-liner:      curl -fsSL https://raw.githubusercontent.com/johnlindquist/gmkit/main/install.sh | bash
#   From a clone:   ./install.sh
#
# By default this downloads prebuilt binaries from GitHub Releases — no Go,
# no Rust, no toolchains required. Pass --source to build from source
# instead (requires Go 1.24+; Rust optional for gmtui).
#
# Env: GMKIT_BIN_DIR overrides the install directory (default ~/.local/bin,
# or your Go bin dir for source builds).
set -euo pipefail

REPO="johnlindquist/gmkit"
REPO_URL="https://github.com/$REPO"
MODULE="github.com/johnlindquist/gmkit"

info() { printf '\033[1;36m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mwarning:\033[0m %s\n' "$*"; }
die() { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

FROM_SOURCE=0
for arg in "${@:-}"; do
  case "$arg" in
    --source) FROM_SOURCE=1 ;;
    "") ;;
    *) die "unknown flag: $arg (supported: --source)" ;;
  esac
done

path_hint() {
  case ":$PATH:" in
    *":$1:"*) ;;
    *) warn "$1 is not on your PATH — add: export PATH=\"\$PATH:$1\"" ;;
  esac
}

quick_start() {
  echo
  info "Done. Quick start:"
  echo "  gmcli auth     # one-time pairing: scan the QR with Google Messages on your phone"
  echo "  gmtui          # browse, search, and approve — the daemon starts automatically"
  echo "  claude mcp add google-messages -- gmcli mcp    # optional: hook up AI agents"
}

# ---------------------------------------------------------------- binaries
install_binaries() {
  local os arch bin_dir tmp
  case "$(uname -s)" in
    Darwin) os="darwin" ;;
    Linux) os="linux" ;;
    *) return 1 ;;
  esac
  case "$(uname -m)" in
    arm64 | aarch64) arch="arm64" ;;
    x86_64 | amd64) arch="amd64" ;;
    *) return 1 ;;
  esac

  bin_dir="${GMKIT_BIN_DIR:-$HOME/.local/bin}"
  mkdir -p "$bin_dir"
  tmp=$(mktemp -d "${TMPDIR:-/tmp}/gmkit.XXXXXX")
  trap 'rm -rf "$tmp"' RETURN

  local base="$REPO_URL/releases/latest/download"
  info "Downloading gmcli (${os}/${arch}) ..."
  curl -fsSL "$base/gmcli_${os}_${arch}.tar.gz" | tar -xz -C "$tmp" || return 1
  install -m 0755 "$tmp/gmcli" "$bin_dir/gmcli"
  info "gmcli installed to $bin_dir/gmcli"

  info "Downloading gmtui (${os}/${arch}) ..."
  if curl -fsSL "$base/gmtui_${os}_${arch}.tar.gz" | tar -xz -C "$tmp"; then
    install -m 0755 "$tmp/gmtui" "$bin_dir/gmtui"
    info "gmtui installed to $bin_dir/gmtui"
  else
    warn "gmtui download failed for ${os}/${arch}; install it with Rust: cargo install --git $REPO_URL gmtui"
  fi

  quick_start
  path_hint "$bin_dir"
}

# ------------------------------------------------------------ from source
install_from_source() {
  command -v go >/dev/null 2>&1 || die "building from source requires Go 1.24+ (https://go.dev/dl/)."
  local go_minor
  go_minor=$(go env GOVERSION | sed -E 's/^go1\.([0-9]+).*/\1/')
  [ "${go_minor:-0}" -ge 24 ] || die "Go 1.24+ required, found $(go env GOVERSION)."

  local script_dir repo_dir version gobin_dir
  script_dir=$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" 2>/dev/null && pwd || true)
  if [ -n "$script_dir" ] && [ -f "$script_dir/go.mod" ] && grep -q "^module $MODULE" "$script_dir/go.mod"; then
    repo_dir="$script_dir"
    info "Building from local clone: $repo_dir"
  else
    command -v git >/dev/null 2>&1 || die "git is required to fetch $REPO_URL."
    repo_dir=$(mktemp -d "${TMPDIR:-/tmp}/gmkit.XXXXXX")
    trap 'rm -rf "$repo_dir"' EXIT
    info "Cloning $REPO_URL ..."
    # Full clone so `git describe --tags` can stamp a real version.
    git clone "$REPO_URL" "$repo_dir"
  fi

  version=$(git -C "$repo_dir" describe --tags --always --dirty 2>/dev/null || echo "dev")
  gobin_dir=$(go env GOBIN)
  [ -n "$gobin_dir" ] || gobin_dir="$(go env GOPATH)/bin"

  info "Building gmcli $version ..."
  (cd "$repo_dir" && go install -ldflags "-X $MODULE/internal/cmd.Version=$version" ./cmd/gmcli)
  info "gmcli installed to $gobin_dir/gmcli"

  if command -v cargo >/dev/null 2>&1; then
    info "Building gmtui (Rust) — first build takes a few minutes ..."
    cargo install --path "$repo_dir/gmtui" --locked
    info "gmtui installed to $HOME/.cargo/bin/gmtui"
  else
    warn "cargo not found — skipping gmtui (the terminal UI)."
    warn "Install Rust from https://rustup.rs then run: cargo install --git $REPO_URL gmtui"
  fi

  quick_start
  path_hint "$gobin_dir"
  command -v cargo >/dev/null 2>&1 && path_hint "$HOME/.cargo/bin"
  return 0
}

if [ "$FROM_SOURCE" = 1 ]; then
  install_from_source
elif ! install_binaries; then
  warn "No prebuilt binaries for this platform (or download failed); building from source."
  install_from_source
fi
