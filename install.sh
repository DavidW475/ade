#!/bin/sh
# Installiert ADE aus einem GitHub-Release.
#
#   curl -fsSL https://raw.githubusercontent.com/DavidW475/ade/main/install.sh | sh
#   curl -fsSL … | sh -s -- --service      # zusätzlich als systemd-User-Dienst einrichten
#
# Umgebungsvariablen:
#   ADE_VERSION      Version, z. B. v1.0.0 (Standard: letztes Release)
#   ADE_INSTALL_DIR  Zielverzeichnis (Standard: ~/.local/bin)
#   ADE_BASE_URL     Basis-URL der Release-Dateien (für Spiegel und Tests)
set -eu

REPO="DavidW475/ade"
service=0

for arg in "$@"; do
	case "$arg" in
	--service) service=1 ;;
	-h | --help)
		cat <<'HILFE'
Installiert ADE aus einem GitHub-Release nach ~/.local/bin.

Optionen:
  --service   zusätzlich als systemd-User-Dienst einrichten (ade install-service)

Umgebungsvariablen:
  ADE_VERSION      Version, z. B. v1.0.0 (Standard: letztes Release)
  ADE_INSTALL_DIR  Zielverzeichnis (Standard: ~/.local/bin)
  ADE_BASE_URL     Basis-URL der Release-Dateien (für Spiegel und Tests)
HILFE
		exit 0
		;;
	*)
		echo "ade-install: unbekannte Option: $arg" >&2
		exit 2
		;;
	esac
done

say() { printf '%s\n' "$*"; }
warn() { printf 'Warnung: %s\n' "$*" >&2; }
die() {
	printf 'ade-install: %s\n' "$*" >&2
	exit 1
}

for tool in curl tar sha256sum uname mktemp; do
	command -v "$tool" >/dev/null 2>&1 || die "$tool fehlt; bitte installieren (z. B. sudo apt install $tool)"
done

[ "$(uname -s)" = "Linux" ] || die "ADE läuft nur unter Linux bzw. WSL2 (gefunden: $(uname -s))"
case "$(uname -m)" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) die "Architektur $(uname -m) wird nicht unterstützt (nur amd64 und arm64)" ;;
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

say "Lade $archive ($version) …"
curl -fsSL --proto '=https,file' -o "$tmp/$archive" "$base/$archive" || die "Download fehlgeschlagen: $base/$archive (gibt es das Release für $version und $arch?)"
curl -fsSL --proto '=https,file' -o "$tmp/checksums.txt" "$base/checksums.txt" || die "Download fehlgeschlagen: $base/checksums.txt"

expected="$(awk -v f="$archive" '$2 == f || $2 == "*" f { print $1 }' "$tmp/checksums.txt")"
[ -n "$expected" ] || die "checksums.txt enthält keine Prüfsumme für $archive"
actual="$(sha256sum "$tmp/$archive" | awk '{ print $1 }')"
[ "$expected" = "$actual" ] || die "Prüfsumme stimmt nicht für $archive (erwartet $expected, erhalten $actual)"

tar -xzf "$tmp/$archive" -C "$tmp" ade || die "$archive enthält kein Binary ade"
mkdir -p "$dir"
# Erst daneben ablegen, dann umbenennen: Ein laufender Daemon behält sein altes Binary.
cp "$tmp/ade" "$dir/.ade.new"
chmod 0755 "$dir/.ade.new"
mv -f "$dir/.ade.new" "$dir/ade"
say "Installiert: $dir/ade ($("$dir/ade" version 2>/dev/null || echo "Version unbekannt"))"

# Voraussetzungen prüfen; fehlende sind nur Warnungen.
if command -v git >/dev/null 2>&1; then
	gv="$(git --version | awk '{ print $3 }')"
	major="${gv%%.*}"
	rest="${gv#*.}"
	minor="${rest%%.*}"
	if [ "$major" -lt 2 ] 2>/dev/null || { [ "$major" -eq 2 ] && [ "$minor" -lt 38 ]; } 2>/dev/null; then
		warn "git $gv ist zu alt, ADE braucht ≥ 2.38"
	fi
else
	warn "git fehlt (sudo apt install git)"
fi
if ! command -v docker >/dev/null 2>&1 && ! command -v podman >/dev/null 2>&1; then
	warn "weder docker noch podman gefunden – Agenten laufen dann nur ohne Sandbox"
fi
command -v gh >/dev/null 2>&1 || warn "gh (GitHub CLI) fehlt – nötig für PRs und Auslöser"
if ! command -v claude >/dev/null 2>&1 && ! command -v codex >/dev/null 2>&1; then
	warn "weder claude (Claude Code) noch codex gefunden – lokal braucht ein Adapter seine CLI"
fi

case ":$PATH:" in
*":$dir:"*) ade="ade" ;;
*)
	ade="$dir/ade"
	say ""
	say "$dir liegt nicht im PATH. Ergänze in ~/.bashrc:"
	say "  export PATH=\"$dir:\$PATH\""
	;;
esac

if [ "$service" = 1 ]; then
	say ""
	"$dir/ade" install-service || die "ade install-service ist gescheitert (siehe oben)"
fi

say ""
say "Fertig. Dashboard öffnen mit:"
say "  $ade start"
