#!/bin/sh
# Reads the newest version from CHANGELOG.md (for automatic releases, auto-release.yml).
#
#   scripts/changelog-version.sh [CHANGELOG.md]
#
# The first heading of the form `## [X.Y.Z] – YYYY-MM-DD` counts (also `X.Y.Z-rc.N`; a hyphen
# instead of the en dash is allowed). `## [Unreleased]` is skipped.
# Output: the version without "v" on stdout, empty if there is none yet.
# Errors (exit 1): a version heading that does not match the format, an invalid date, or a top
# version that is not newer than the one below it.
# Needs GNU date and sort (as on the GitHub runners and under WSL).
set -eu

file=${1:-CHANGELOG.md}
if [ ! -f "$file" ]; then
	echo "changelog-version: $file is missing" >&2
	exit 1
fi

semver='[0-9]\{1,\}\.[0-9]\{1,\}\.[0-9]\{1,\}\(-[0-9A-Za-z][0-9A-Za-z.-]*\)\{0,1\}'
re="^## \[\($semver\)\] \(–\|-\) \([0-9]\{4\}-[0-9]\{2\}-[0-9]\{2\}\)$"

# newer A B: succeeds if A is newer than B by SemVer. A pre-release is older than the full
# release with the same core (1.0.0-rc.1 < 1.0.0).
newer() {
	[ "$1" != "$2" ] || return 1
	core_a=${1%%-*} core_b=${2%%-*}
	if [ "$core_a" != "$core_b" ]; then
		[ "$(printf '%s\n%s\n' "$core_a" "$core_b" | sort -t. -k1,1n -k2,2n -k3,3n | tail -n 1)" = "$core_a" ]
		return
	fi
	pre_a=${1#"$core_a"} pre_b=${2#"$core_b"}
	[ -n "$pre_a" ] || return 0 # A is the full release
	[ -n "$pre_b" ] || return 1
	[ "$(printf '%s\n%s\n' "$pre_a" "$pre_b" | sort -V | tail -n 1)" = "$pre_a" ]
}

newest="" previous="" n=0
lines=$(grep -n '^## \[' "$file" | grep -v '^[0-9]*:## \[Unreleased\]$' || true)
while IFS= read -r entry; do
	[ -n "$entry" ] || continue
	line=${entry%%:*}
	text=${entry#*:}
	v=$(printf '%s\n' "$text" | sed -n "s/$re/\1/p")
	d=$(printf '%s\n' "$text" | sed -n "s/$re/\4/p")
	if [ -z "$v" ]; then
		echo "changelog-version: $file:$line: expected \"## [X.Y.Z] – YYYY-MM-DD\", found: $text" >&2
		exit 1
	fi
	if [ "$(date -u -d "$d" +%F 2>/dev/null || true)" != "$d" ]; then
		echo "changelog-version: $file:$line: invalid date $d" >&2
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
	echo "changelog-version: $file: $newest is not newer than $previous below it" >&2
	exit 1
fi
printf '%s\n' "$newest"
