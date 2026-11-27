#!/bin/sh
# check-dist.sh DIR: checks a GoReleaser output directory holds exactly the
# release artifacts: one archive per release target, a checksums.txt that
# lists every archive with a matching sha256, and one SBOM per archive.
# When any .sigstore.json bundle is present (a real, signed release), every
# archive and checksums.txt must have one.
set -eu

dir=${1:?usage: check-dist.sh DIR}
targets="darwin_amd64 darwin_arm64 linux_amd64 linux_arm64 windows_amd64 windows_arm64"

fail() {
	echo "check-dist: $*" >&2
	exit 1
}

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	else
		shasum -a 256 "$1" | cut -d' ' -f1
	fi
}

[ -f "$dir/checksums.txt" ] || fail "no checksums.txt in $dir"

signed=no
if ls "$dir"/*.sigstore.json >/dev/null 2>&1; then
	signed=yes
	[ -f "$dir/checksums.txt.sigstore.json" ] || fail "no signature bundle for checksums.txt"
fi

n=0
for t in $targets; do
	case $t in windows_*) ext=zip ;; *) ext=tar.gz ;; esac
	set -- "$dir"/gaia_*_"$t".$ext
	[ $# -eq 1 ] && [ -f "$1" ] || fail "want exactly one $t archive, found: $*"
	a=$1
	name=$(basename "$a")
	want=$(awk -v f="$name" '$2 == f { print $1 }' "$dir/checksums.txt")
	[ -n "$want" ] || fail "$name is not in checksums.txt"
	[ "$want" = "$(sha256 "$a")" ] || fail "$name does not match its checksum"
	[ -s "$a.sbom.json" ] || fail "no SBOM for $name"
	if [ $signed = yes ]; then
		[ -f "$a.sigstore.json" ] || fail "no signature bundle for $name"
	fi
	n=$((n + 1))
done

archives=$(ls "$dir" | grep -cE '\.(tar\.gz|zip)$' || true)
[ "$archives" -eq 6 ] || fail "want 6 archives, found $archives"
echo "check-dist: $n archives, checksums.txt and $n SBOMs OK (signed: $signed)"
