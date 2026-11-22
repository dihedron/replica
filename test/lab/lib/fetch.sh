# shellcheck shell=bash
# Downloads, cached on the lab host and verified as an operator would
# (.specs/veho-lab.md, LAB-3): etcd against the release's SHA256SUMS, Garage
# against the SHA-256 pinned in lab.env (Garage publishes no checksums), the
# PGDG signing key against its pinned fingerprint. Copied from ligo 9f11da9
# (test/lab/lib/fetch.sh): fetch is unchanged, the release helpers are new.

# fetch URL SHA256: downloads URL into the cache unless already there with
# that checksum, and prints the cached path. Fails on a mismatch.
fetch() {
	local url="$1" sha="$2" path
	path="$LAB_CACHE_DIR/downloads/${url##*/}"
	mkdir -p "${path%/*}"
	if [[ -f "$path" ]] && echo "$sha  $path" | sha256sum -c --quiet >/dev/null 2>&1; then
		echo "$path"
		return
	fi
	info "downloading ${url##*/}" >&2
	curl -fsSL --proto '=https' -o "$path.part" "$url" || die "cannot download $url"
	echo "$sha  $path.part" | sha256sum -c --quiet >/dev/null 2>&1 || {
		rm -f "$path.part"
		die "checksum mismatch for $url (expected $sha)"
	}
	mv "$path.part" "$path"
	echo "$path"
}

# etcd_release: the cached, verified etcd tarball of LAB_ETCD_VERSION.
etcd_release() {
	local base="https://github.com/etcd-io/etcd/releases/download/v$LAB_ETCD_VERSION"
	local file="etcd-v$LAB_ETCD_VERSION-linux-amd64.tar.gz" sha
	sha="$(curl -fsSL --proto '=https' "$base/SHA256SUMS" | awk -v f="$file" '$2 == f {print $1}')"
	[[ "$sha" =~ ^[0-9a-f]{64}$ ]] || die "no published SHA-256 for $file"
	fetch "$base/$file" "$sha"
}

# garage_binary: the cached Garage binary of LAB_GARAGE_VERSION, checked
# against LAB_GARAGE_SHA256. The file name carries the version: every release
# is called "garage".
garage_binary() {
	local url="https://garagehq.deuxfleurs.fr/_releases/v$LAB_GARAGE_VERSION/x86_64-unknown-linux-musl/garage"
	local path="$LAB_CACHE_DIR/downloads/garage-$LAB_GARAGE_VERSION"
	mkdir -p "${path%/*}"
	if ! { [[ -f "$path" ]] && echo "$LAB_GARAGE_SHA256  $path" | sha256sum -c --quiet >/dev/null 2>&1; }; then
		info "downloading garage $LAB_GARAGE_VERSION" >&2
		curl -fsSL --proto '=https' -o "$path.part" "$url" || die "cannot download $url"
		echo "$LAB_GARAGE_SHA256  $path.part" | sha256sum -c --quiet >/dev/null 2>&1 || {
			rm -f "$path.part"
			die "checksum mismatch for $url (expected $LAB_GARAGE_SHA256)"
		}
		mv "$path.part" "$path"
	fi
	echo "$path"
}

# pgdg_key: the cached PGDG archive key, checked against
# LAB_PGDG_KEY_FINGERPRINT before it is used.
pgdg_key() {
	local path="$LAB_CACHE_DIR/downloads/pgdg-ACCC4CF8.asc" fpr
	mkdir -p "${path%/*}"
	if [[ ! -f "$path" ]]; then
		curl -fsSL --proto '=https' -o "$path.part" https://www.postgresql.org/media/keys/ACCC4CF8.asc ||
			die "cannot download the PGDG signing key"
		mv "$path.part" "$path"
	fi
	fpr="$(gpg --show-keys --with-colons "$path" 2>/dev/null | awk -F: '!found && $1 == "fpr" {print $10; found = 1}')"
	[[ "$fpr" == "$LAB_PGDG_KEY_FINGERPRINT" ]] ||
		die "the PGDG signing key is $fpr, not the pinned $LAB_PGDG_KEY_FINGERPRINT ($path)"
	echo "$path"
}
