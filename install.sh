#!/usr/bin/env bash
# gmkit installer — builds and globally installs gmcli (Go) and gmtui (Rust).
#
#   From a clone:   ./install.sh
#   One-liner:      curl -fsSL https://raw.githubusercontent.com/johnlindquist/gmkit/main/install.sh | bash
#
# Requirements: Go 1.24+. Rust (cargo) is optional — without it, gmtui is
# skipped and only the gmcli CLI/daemon/MCP server is installed.
set -euo pipefail

REPO_URL="https://github.com/johnlindquist/gmkit"
MODULE="github.com/johnlindquist/gmkit"

info() { printf '\033[1;36m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mwarning:\033[0m %s\n' "$*"; }
die() { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

command -v go >/dev/null 2>&1 || die "Go is required (https://go.dev/dl/). gmkit needs Go 1.24+."

GO_MINOR=$(go env GOVERSION | sed -E 's/^go1\.([0-9]+).*/\1/')
[ "${GO_MINOR:-0}" -ge 24 ] || die "Go 1.24+ required, found $(go env GOVERSION)."

# Locate the repo: use the clone this script lives in, otherwise clone fresh.
SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" 2>/dev/null && pwd || true)
if [ -n "$SCRIPT_DIR" ] && [ -f "$SCRIPT_DIR/go.mod" ] && grep -q "^module $MODULE" "$SCRIPT_DIR/go.mod"; then
  REPO_DIR="$SCRIPT_DIR"
  info "Installing from local clone: $REPO_DIR"
else
  command -v git >/dev/null 2>&1 || die "git is required to fetch $REPO_URL."
  REPO_DIR=$(mktemp -d "${TMPDIR:-/tmp}/gmkit.XXXXXX")
  trap 'rm -rf "$REPO_DIR"' EXIT
  info "Cloning $REPO_URL ..."
  git clone --depth 1 "$REPO_URL" "$REPO_DIR"
fi

VERSION=$(git -C "$REPO_DIR" describe --tags --always --dirty 2>/dev/null || echo "dev")
GOBIN_DIR=$(go env GOBIN)
[ -n "$GOBIN_DIR" ] || GOBIN_DIR="$(go env GOPATH)/bin"

info "Building gmcli $VERSION ..."
(cd "$REPO_DIR" && go install -ldflags "-X $MODULE/internal/cmd.Version=$VERSION" ./cmd/gmcli)
info "gmcli installed to $GOBIN_DIR/gmcli"

if command -v cargo >/dev/null 2>&1; then
  info "Building gmtui (Rust) — first build takes a few minutes ..."
  cargo install --path "$REPO_DIR/gmtui" --locked
  info "gmtui installed to $HOME/.cargo/bin/gmtui"
else
  warn "cargo not found — skipping gmtui (the terminal UI)."
  warn "Install Rust from https://rustup.rs then run: cargo install --git $REPO_URL gmtui"
fi

echo
info "Done. Quick start:"
echo "  gmcli auth     # one-time pairing: scan the QR with Google Messages on your phone"
echo "  gmtui          # browse, search, and approve — the daemon starts automatically"
echo "  claude mcp add google-messages -- gmcli mcp    # optional: hook up AI agents"

case ":$PATH:" in
  *":$GOBIN_DIR:"*) ;;
  *) warn "$GOBIN_DIR is not on your PATH — add: export PATH=\"\$PATH:$GOBIN_DIR\"" ;;
esac
if command -v cargo >/dev/null 2>&1; then
  case ":$PATH:" in
    *":$HOME/.cargo/bin:"*) ;;
    *) warn "$HOME/.cargo/bin is not on your PATH — add: export PATH=\"\$PATH:\$HOME/.cargo/bin\"" ;;
  esac
fi
