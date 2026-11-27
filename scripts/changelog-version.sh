#!/bin/sh
# Liest die neueste Version aus CHANGELOG.md (für automatische Releases, auto-release.yml).
#
#   scripts/changelog-version.sh [CHANGELOG.md]
#
# Maßgeblich ist die erste Überschrift der Form `## [X.Y.Z] – JJJJ-MM-TT` (auch `X.Y.Z-rc.N`;
# Bindestrich statt Gedankenstrich ist erlaubt). `## [Unveröffentlicht]` wird übersprungen.
# Ausgabe: die Version ohne "v" auf stdout, leer, wenn es noch keine gibt.
# Fehler (Exit 1): eine Versionsüberschrift, die nicht dem Format entspricht, ein ungültiges
# Datum oder eine oberste Version, die nicht neuer ist als die darunter.
# Braucht GNU date und sort (wie auf den GitHub-Runnern und unter WSL).
set -eu

file=${1:-CHANGELOG.md}
if [ ! -f "$file" ]; then
	echo "changelog-version: $file fehlt" >&2
	exit 1
fi

semver='[0-9]\{1,\}\.[0-9]\{1,\}\.[0-9]\{1,\}\(-[0-9A-Za-z][0-9A-Za-z.-]*\)\{0,1\}'
re="^## \[\($semver\)\] \(–\|-\) \([0-9]\{4\}-[0-9]\{2\}-[0-9]\{2\}\)$"

# newer A B: Erfolg, wenn A nach SemVer neuer ist als B. Eine Vorabversion ist älter als die
# Vollversion mit gleichem Kern (1.0.0-rc.1 < 1.0.0).
newer() {
	[ "$1" != "$2" ] || return 1
	core_a=${1%%-*} core_b=${2%%-*}
	if [ "$core_a" != "$core_b" ]; then
		[ "$(printf '%s\n%s\n' "$core_a" "$core_b" | sort -t. -k1,1n -k2,2n -k3,3n | tail -n 1)" = "$core_a" ]
		return
	fi
	pre_a=${1#"$core_a"} pre_b=${2#"$core_b"}
	[ -n "$pre_a" ] || return 0 # A ist die Vollversion
	[ -n "$pre_b" ] || return 1
	[ "$(printf '%s\n%s\n' "$pre_a" "$pre_b" | sort -V | tail -n 1)" = "$pre_a" ]
}

newest="" previous="" n=0
lines=$(grep -n '^## \[' "$file" | grep -v '^[0-9]*:## \[Unveröffentlicht\]$' || true)
while IFS= read -r entry; do
	[ -n "$entry" ] || continue
	line=${entry%%:*}
	text=${entry#*:}
	v=$(printf '%s\n' "$text" | sed -n "s/$re/\1/p")
	d=$(printf '%s\n' "$text" | sed -n "s/$re/\4/p")
	if [ -z "$v" ]; then
		echo "changelog-version: $file:$line: erwartet \"## [X.Y.Z] – JJJJ-MM-TT\", gefunden: $text" >&2
		exit 1
	fi
	if [ "$(date -u -d "$d" +%F 2>/dev/null || true)" != "$d" ]; then
		echo "changelog-version: $file:$line: ungültiges Datum $d" >&2
		exit 1
	fi
	n=$((n + 1))
	case $n in
	1) newest=$v ;;
	2) previous=$v ;;
	esac
done <<EOF
$lines
EOF

if [ -n "$previous" ] && ! newer "$newest" "$previous"; then
	echo "changelog-version: $file: $newest ist nicht neuer als $previous darunter" >&2
	exit 1
fi
printf '%s\n' "$newest"
