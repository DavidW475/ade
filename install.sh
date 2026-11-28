#!/bin/sh
# Installs ADE from a GitHub release.
#
#   curl -fsSL https://raw.githubusercontent.com/DavidW475/ade/main/install.sh | sh
#   curl -fsSL … | sh -s -- --service      # also set it up as a systemd user service
#
# Environment variables:
#   ADE_VERSION      version, e.g. v1.0.0 (default: latest release)
#   ADE_INSTALL_DIR  target directory (default: ~/.local/bin)
#   ADE_BASE_URL     base URL of the release files (for mirrors and tests)
set -eu

REPO="DavidW475/ade"
service=0

for arg in "$@"; do
	case "$arg" in
	--service) service=1 ;;
	-h | --help)
		cat <<'HELP'
Installs ADE from a GitHub release to ~/.local/bin.

Options:
  --service   also set it up as a systemd user service (ade install-service)

Environment variables:
  ADE_VERSION      version, e.g. v1.0.0 (default: latest release)
  ADE_INSTALL_DIR  target directory (default: ~/.local/bin)
  ADE_BASE_URL     base URL of the release files (for mirrors and tests)
HELP
		exit 0
		;;
	*)
		echo "ade-install: unknown option: $arg" >&2
		exit 2
		;;
	esac
done

say() { printf '%s\n' "$*"; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die() {
	printf 'ade-install: %s\n' "$*" >&2
	exit 1
}

for tool in curl tar sha256sum uname mktemp; do
	command -v "$tool" >/dev/null 2>&1 || die "$tool is missing; please install it (e.g. sudo apt install $tool)"
done

[ "$(uname -s)" = "Linux" ] || die "ADE only runs on Linux or WSL2 (found: $(uname -s))"
case "$(uname -m)" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) die "architecture $(uname -m) is not supported (only amd64 and arm64)" ;;
esac

version="${ADE_VERSION:-latest}"
if [ -n "${ADE_BASE_URL:-}" ]; then
	base="$ADE_BASE_URL"
elif [ "$version" = "latest" ]; then
	base="https://github.com/$REPO/releases/latest/download"
else
	case "$version" in v*) ;; *) version="v$version" ;; esac
	base="https://github.com/$REPO/releases/download/$version"
fi
dir="${ADE_INSTALL_DIR:-$HOME/.local/bin}"
archive="ade_linux_${arch}.tar.gz"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT INT TERM

say "Downloading $archive ($version) …"
curl -fsSL --proto '=https,file' -o "$tmp/$archive" "$base/$archive" || die "download failed: $base/$archive (does the release exist for $version and $arch?)"
curl -fsSL --proto '=https,file' -o "$tmp/checksums.txt" "$base/checksums.txt" || die "download failed: $base/checksums.txt"

expected="$(awk -v f="$archive" '$2 == f || $2 == "*" f { print $1 }' "$tmp/checksums.txt")"
[ -n "$expected" ] || die "checksums.txt contains no checksum for $archive"
actual="$(sha256sum "$tmp/$archive" | awk '{ print $1 }')"
[ "$expected" = "$actual" ] || die "checksum does not match for $archive (expected $expected, got $actual)"

tar -xzf "$tmp/$archive" -C "$tmp" ade || die "$archive contains no binary ade"
mkdir -p "$dir"
# Put it next to the old one first, then rename: a running daemon keeps its old binary.
cp "$tmp/ade" "$dir/.ade.new"
chmod 0755 "$dir/.ade.new"
mv -f "$dir/.ade.new" "$dir/ade"
say "Installed: $dir/ade ($("$dir/ade" version 2>/dev/null || echo "version unknown"))"

# Check the requirements; missing ones are only warnings.
if command -v git >/dev/null 2>&1; then
	gv="$(git --version | awk '{ print $3 }')"
	major="${gv%%.*}"
	rest="${gv#*.}"
	minor="${rest%%.*}"
	if [ "$major" -lt 2 ] 2>/dev/null || { [ "$major" -eq 2 ] && [ "$minor" -lt 38 ]; } 2>/dev/null; then
		warn "git $gv is too old, ADE needs ≥ 2.38"
	fi
else
	warn "git is missing (sudo apt install git)"
fi
if ! command -v docker >/dev/null 2>&1 && ! command -v podman >/dev/null 2>&1; then
	warn "neither docker nor podman found – agents then only run without a sandbox"
fi
command -v gh >/dev/null 2>&1 || warn "gh (GitHub CLI) is missing – needed for PRs and triggers"
if ! command -v claude >/dev/null 2>&1 && ! command -v codex >/dev/null 2>&1; then
	warn "neither claude (Claude Code) nor codex found – locally an adapter needs its CLI"
fi

case ":$PATH:" in
*":$dir:"*) ade="ade" ;;
*)
	ade="$dir/ade"
	say ""
	say "$dir is not in the PATH. Add to ~/.bashrc:"
	say "  export PATH=\"$dir:\$PATH\""
	;;
esac

if [ "$service" = 1 ]; then
	say ""
	"$dir/ade" install-service || die "ade install-service failed (see above)"
fi

say ""
say "Done. Open the dashboard with:"
say "  $ade start"
