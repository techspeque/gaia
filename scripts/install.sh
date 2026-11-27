#!/bin/sh
# install.sh: download a Gaia release, verify it, and install the gaia binary
# for the current user. No root is needed or used.
#
#   curl -fsSL https://raw.githubusercontent.com/techspeque/gaia/main/scripts/install.sh | sh
#
# Verification, before anything is installed (ADR-0012 rule 3):
#   1. the keyless cosign signature of checksums.txt, when cosign is on PATH;
#   2. the archive's sha256 against checksums.txt.
# Any failure exits non-zero and installs nothing.
#
# Environment:
#   GAIA_VERSION            release to install, e.g. v0.1.0 (default: latest)
#   GAIA_INSTALL_DIR        where to put gaia (default: $HOME/.local/bin)
#   GAIA_REQUIRE_SIGNATURE  1: refuse to install when cosign is not available
#   GAIA_RELEASES_URL       release base URL (default: the GitHub releases page)
set -eu

repo=techspeque/gaia
base=${GAIA_RELEASES_URL:-https://github.com/$repo/releases}
install_dir=${GAIA_INSTALL_DIR:-${HOME:?HOME is not set}/.local/bin}
# The signing identity: this repository's release workflow, run for a tag.
cert_identity="^https://github\.com/$repo/\.github/workflows/release\.yml@refs/tags/v"
cert_issuer=https://token.actions.githubusercontent.com
# Downloads use HTTPS only, unless the base URL is itself plain http (tests).
proto='=https'
case $base in http://*) proto='=http' ;; esac

say() { echo "install.sh: $*" >&2; }
fail() {
	say "$*"
	say "nothing was installed"
	exit 1
}

need() { command -v "$1" >/dev/null 2>&1 || fail "$1 is required"; }

detect_platform() {
	case $(uname -s) in
	Darwin) os=darwin ;;
	Linux) os=linux ;;
	*) fail "unsupported OS $(uname -s): Gaia installs on darwin and linux" ;;
	esac
	case $(uname -m) in
	x86_64 | amd64) arch=amd64 ;;
	arm64 | aarch64) arch=arm64 ;;
	*) fail "unsupported architecture $(uname -m): Gaia installs on amd64 and arm64" ;;
	esac
}

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | cut -d' ' -f1
	else
		fail "sha256sum or shasum is required to verify the download"
	fi
}

fetch() { # URL FILE
	curl -fsSL --proto "$proto" -o "$2" "$1" || fail "download failed: $1"
}

resolve_version() {
	if [ -n "${GAIA_VERSION:-}" ]; then
		tag=v${GAIA_VERSION#v}
		return
	fi
	url=$(curl -fsSLI --proto "$proto" -o /dev/null -w '%{url_effective}' "$base/latest") ||
		fail "cannot find the latest release at $base/latest"
	tag=${url##*/}
	case $tag in
	v[0-9]*) ;;
	*) fail "cannot find the latest release at $base/latest (got $url)" ;;
	esac
}

verify_signature() { # FILE BUNDLE
	if ! command -v cosign >/dev/null 2>&1; then
		if [ "${GAIA_REQUIRE_SIGNATURE:-}" = 1 ]; then
			fail "cosign is not installed and GAIA_REQUIRE_SIGNATURE=1: cannot verify the signature"
		fi
		say "warning: cosign is not installed, so the signature was NOT verified; only the checksum is"
		return
	fi
	cosign verify-blob \
		--bundle "$2" \
		--certificate-identity-regexp "$cert_identity" \
		--certificate-oidc-issuer "$cert_issuer" \
		"$1" >/dev/null 2>&1 || fail "signature verification failed for checksums.txt"
	say "signature of checksums.txt verified"
}

main() {
	need curl
	need tar
	detect_platform
	resolve_version
	version=${tag#v}
	archive=gaia_${version}_${os}_${arch}.tar.gz

	tmp=$(mktemp -d "${TMPDIR:-/tmp}/gaia-install.XXXXXX")
	trap 'rm -rf "$tmp"' EXIT
	trap 'exit 130' INT TERM
	chmod 700 "$tmp"

	say "downloading gaia $tag for $os/$arch"
	fetch "$base/download/$tag/$archive" "$tmp/$archive"
	fetch "$base/download/$tag/checksums.txt" "$tmp/checksums.txt"
	if command -v cosign >/dev/null 2>&1; then
		fetch "$base/download/$tag/checksums.txt.sigstore.json" "$tmp/checksums.txt.sigstore.json"
	fi

	verify_signature "$tmp/checksums.txt" "$tmp/checksums.txt.sigstore.json"

	want=$(awk -v f="$archive" '$2 == f { print $1 }' "$tmp/checksums.txt")
	[ -n "$want" ] || fail "$archive is not listed in checksums.txt"
	got=$(sha256 "$tmp/$archive")
	[ "$want" = "$got" ] || fail "checksum mismatch for $archive: want $want, got $got"
	say "checksum of $archive verified"

	mkdir "$tmp/x"
	tar -xzf "$tmp/$archive" -C "$tmp/x" gaia || fail "$archive holds no gaia binary"
	[ -f "$tmp/x/gaia" ] || fail "$archive holds no gaia binary"

	mkdir -p "$install_dir"
	cp "$tmp/x/gaia" "$install_dir/.gaia.new.$$"
	chmod 755 "$install_dir/.gaia.new.$$"
	mv -f "$install_dir/.gaia.new.$$" "$install_dir/gaia"
	say "installed $install_dir/gaia ($tag)"

	case :${PATH}: in
	*:"$install_dir":*) ;;
	*) say "$install_dir is not on your PATH; add it to run gaia" ;;
	esac
}

main "$@"
