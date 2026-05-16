#!/bin/sh
# CoreMCP installer — POSIX sh, Linux + macOS, amd64/arm64.
#
# Usage:
#   curl -fsSL https://get.corebasehq.com | sh
#
# Env vars:
#   VERSION       Override release tag (default: latest)
#   INSTALL_DIR   Override install path (default: /usr/local/bin or ~/.local/bin)
#   COREMCP_REPO  Override repo (default: corebasehq/coremcp)

set -eu

REPO="${COREMCP_REPO:-corebasehq/coremcp}"
VERSION="${VERSION:-latest}"

if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  BOLD="$(printf '\033[1m')"
  DIM="$(printf '\033[2m')"
  RED="$(printf '\033[31m')"
  GREEN="$(printf '\033[32m')"
  RESET="$(printf '\033[0m')"
else
  BOLD=""; DIM=""; RED=""; GREEN=""; RESET=""
fi

info() { printf '%s>%s %s\n' "$BOLD" "$RESET" "$1" >&2; }
ok()   { printf '%s✓%s %s\n' "$GREEN" "$RESET" "$1" >&2; }
die()  { printf '%s✗%s %s\n' "$RED" "$RESET" "$1" >&2; exit 1; }

detect_os() {
  case "$(uname -s)" in
    Linux)  echo linux ;;
    Darwin) echo darwin ;;
    *) die "unsupported OS: $(uname -s). Linux and macOS only." ;;
  esac
}

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64) echo amd64 ;;
    aarch64|arm64) echo arm64 ;;
    *) die "unsupported arch: $(uname -m). Build from source for this platform." ;;
  esac
}

have() { command -v "$1" >/dev/null 2>&1; }

fetch() {
  if have curl; then
    curl -fsSL "$1"
  elif have wget; then
    wget -q -O- "$1"
  else
    die "neither curl nor wget found. Install one and retry."
  fi
}

fetch_to() {
  if have curl; then
    curl -fsSL -o "$2" "$1"
  elif have wget; then
    wget -q -O "$2" "$1"
  else
    die "neither curl nor wget found."
  fi
}

resolve_version() {
  if [ "$VERSION" = "latest" ]; then
    info "resolving latest version…"
    if have curl; then
      RESOLVED="$(curl -fsSLI -o /dev/null -w '%{url_effective}' \
        "https://github.com/$REPO/releases/latest" \
        | sed 's|.*/tag/||')"
    else
      RESOLVED="$(wget --max-redirect=0 -S \
        "https://github.com/$REPO/releases/latest" 2>&1 \
        | sed -n 's|.*Location:.*/tag/\([^[:space:]]*\).*|\1|p' | tail -1)"
    fi
    [ -n "$RESOLVED" ] || die "couldn't resolve latest version. Set VERSION=vX.Y.Z and retry."
    echo "$RESOLVED"
  else
    echo "$VERSION"
  fi
}

pick_install_dir() {
  if [ -n "${INSTALL_DIR:-}" ]; then
    mkdir -p "$INSTALL_DIR" 2>/dev/null || die "INSTALL_DIR=$INSTALL_DIR is not writable."
    echo "$INSTALL_DIR"
    return
  fi

  if [ -w /usr/local/bin ] || [ "$(id -u)" = "0" ]; then
    echo /usr/local/bin
    return
  fi

  local_dir="$HOME/.local/bin"
  mkdir -p "$local_dir"
  echo "$local_dir"
}

OS="$(detect_os)"
ARCH="$(detect_arch)"
TAG="$(resolve_version)"
DIR="$(pick_install_dir)"

ASSET="coremcp-${OS}-${ARCH}"
URL="https://github.com/${REPO}/releases/download/${TAG}/${ASSET}"

info "platform:      ${BOLD}${OS}/${ARCH}${RESET}"
info "version:       ${BOLD}${TAG}${RESET}"
info "install dir:   ${BOLD}${DIR}${RESET}"
info "downloading:   ${DIM}${URL}${RESET}"

TMP="$(mktemp -d 2>/dev/null || mktemp -d -t coremcp)"
trap 'rm -rf "$TMP"' EXIT INT TERM

fetch_to "$URL" "$TMP/coremcp" || die "download failed. Check that ${TAG} has a ${ASSET} asset."

SUMS_URL="https://github.com/${REPO}/releases/download/${TAG}/checksums.txt"
SUMTOOL=""
if have sha256sum; then
  SUMTOOL="sha256sum"
elif have shasum; then
  SUMTOOL="shasum -a 256"
fi
if [ -n "$SUMTOOL" ] && fetch "$SUMS_URL" >"$TMP/checksums.txt" 2>/dev/null; then
  info "verifying sha256…"
  EXPECTED="$(awk -v f="$ASSET" '$2 == f {print $1}' "$TMP/checksums.txt" | head -1)"
  if [ -n "$EXPECTED" ]; then
    ACTUAL="$($SUMTOOL "$TMP/coremcp" | awk '{print $1}')"
    [ "$EXPECTED" = "$ACTUAL" ] || die "checksum mismatch. expected=$EXPECTED actual=$ACTUAL"
    ok "checksum verified"
  fi
fi

chmod +x "$TMP/coremcp"

TARGET="$DIR/coremcp"
if [ -w "$DIR" ]; then
  mv "$TMP/coremcp" "$TARGET"
elif have sudo; then
  info "elevating with sudo to write $TARGET"
  sudo mv "$TMP/coremcp" "$TARGET"
else
  die "$DIR is not writable and sudo is not available. Set INSTALL_DIR=\$HOME/.local/bin and retry."
fi

ok "installed: ${BOLD}${TARGET}${RESET}"

add_path_to_profile() {
  dir="$1"
  profile="$2"
  fmt="$3"

  mkdir -p "$(dirname "$profile")"
  [ -f "$profile" ] || touch "$profile"

  if grep -Fq "# >>> coremcp PATH >>>" "$profile" 2>/dev/null; then
    return 0
  fi

  {
    printf '\n# >>> coremcp PATH >>>\n'
    # shellcheck disable=SC2059
    printf "$fmt\n" "$dir"
    printf '# <<< coremcp PATH <<<\n'
  } >>"$profile"

  ok "added $dir to PATH in $profile"
}

case ":$PATH:" in
  *":$DIR:"*) ;;
  *)
    shell_name="$(basename "${SHELL:-sh}")"
    updated=0

    case "$shell_name" in
      bash)
        if [ "$OS" = "darwin" ]; then
          add_path_to_profile "$DIR" "$HOME/.bash_profile" 'export PATH="%s:$PATH"'
        else
          add_path_to_profile "$DIR" "$HOME/.bashrc" 'export PATH="%s:$PATH"'
        fi
        updated=1
        ;;
      zsh)
        add_path_to_profile "$DIR" "${ZDOTDIR:-$HOME}/.zshrc" 'export PATH="%s:$PATH"'
        updated=1
        ;;
      fish)
        add_path_to_profile "$DIR" "$HOME/.config/fish/config.fish" 'fish_add_path %s'
        updated=1
        ;;
    esac

    if [ "$updated" = "1" ]; then
      printf '\n%s!%s Open a new shell or run: %ssource %s%s\n\n' \
        "$BOLD" "$RESET" "$DIM" "$profile" "$RESET" >&2
    else
      printf '\n%s!%s %s is not in your PATH. Add this to your shell profile:\n' \
        "$RED" "$RESET" "$DIR" >&2
      printf '    export PATH="%s:$PATH"\n\n' "$DIR" >&2
    fi
    ;;
esac

cat <<EOF

${BOLD}Next steps${RESET}
  1. Create a config:   ${DIM}coremcp init > coremcp.yaml${RESET}
  2. Edit the YAML to point at your database
  3. Wire to Claude Desktop / Cursor — see ${DIM}https://docs.corebasehq.com/coremcp/quickstart${RESET}

Run ${BOLD}coremcp --help${RESET} for available commands.
EOF
