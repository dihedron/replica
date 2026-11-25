# shellcheck shell=bash
# Common functions of the DevStack lab: settings, output, downloads and the
# lab's state. Sourced by lab.sh, which sets LAB_DIR (test/devstack) and
# REPO_DIR (the repository). Adapted from openstack-spiffe's lab, with no
# reference to it.

# load_settings reads lab.env, then lab.local.env if present, then gives
# precedence to the LAB_* variables set in the environment.
load_settings() {
	local name pair saved=()
	for name in $(compgen -A export LAB_); do
		saved+=("$name=${!name}")
	done
	# shellcheck disable=SC1091 # lab.env: settings, checked on its own
	source "$LAB_DIR/lab.env"
	if [[ -f "$LAB_DIR/lab.local.env" ]]; then
		# shellcheck disable=SC1091
		source "$LAB_DIR/lab.local.env"
	fi
	for pair in "${saved[@]}"; do
		printf -v "${pair%%=*}" '%s' "${pair#*=}"
	done
	[[ "$LAB_STATE_DIR" = /* ]] || LAB_STATE_DIR="$LAB_DIR/$LAB_STATE_DIR"
}

# --- Output -------------------------------------------------------------------

if [[ -t 1 ]]; then
	_red=$'\e[31m' _green=$'\e[32m' _bold=$'\e[1m' _reset=$'\e[0m'
else
	_red='' _green='' _bold='' _reset=''
fi

section() { printf '\n%s%s%s\n' "$_bold" "$*" "$_reset"; }
info() { printf '        %s\n' "$*"; }
ok() { printf '  %sok%s    %s\n' "$_green" "$_reset" "$*"; }
die() {
	printf '%serror:%s %s\n' "$_red" "$_reset" "$*" >&2
	exit 1
}

have() { command -v "$1" >/dev/null 2>&1; }

# confirm QUESTION: yes from the terminal, or LAB_ASSUME_YES.
confirm() {
	[[ -n "$LAB_ASSUME_YES" ]] && return 0
	[[ -r /dev/tty ]] || return 1
	local answer
	read -r -p "$1 [y/N] " answer </dev/tty || return 1
	[[ "$answer" == [yY]* ]]
}

# lab_ip HOST_PART: the address of a host in LAB_SUBNET.
lab_ip() {
	local base="${LAB_SUBNET%/*}"
	echo "${base%.*}.$1"
}

# --- Versions and downloads ---------------------------------------------------

ubuntu_image_url() {
	if [[ -n "$LAB_UBUNTU_IMAGE_URL" ]]; then
		echo "$LAB_UBUNTU_IMAGE_URL"
		return
	fi
	local codename
	case "$LAB_UBUNTU_RELEASE" in
	22.04) codename=jammy ;;
	24.04) codename=noble ;;
	26.04) codename=resolute ;;
	*) die "no known image for Ubuntu $LAB_UBUNTU_RELEASE: set LAB_UBUNTU_IMAGE_URL" ;;
	esac
	echo "https://cloud-images.ubuntu.com/$codename/current/$codename-server-cloudimg-amd64.img"
}

# resolve_devstack_branch: LAB_DEVSTACK_BRANCH, or DevStack's most recent
# stable/YYYY.N branch.
resolve_devstack_branch() {
	if [[ -n "$LAB_DEVSTACK_BRANCH" ]]; then
		echo "$LAB_DEVSTACK_BRANCH"
		return
	fi
	local branch
	branch="$(git ls-remote --heads https://opendev.org/openstack/devstack 'refs/heads/stable/*' |
		awk '{ sub("refs/heads/", "", $2); print $2 }' |
		grep -E '^stable/[0-9]{4}\.[0-9]+$' | sort -V | tail -n1)"
	[[ -n "$branch" ]] || die "cannot list DevStack's stable branches"
	echo "$branch"
}

# published_sha256 URL: the SHA-256 of URL from the SHA256SUMS next to it.
published_sha256() {
	local dir="${1%/*}" file="${1##*/}"
	curl -fsSL "$dir/SHA256SUMS" 2>/dev/null | awk -v f="$file" '
		$2 == f || $2 == "*" f { print $1; found = 1; exit }
		END { exit !found }'
}

# fetch URL SHA256: the cached copy of URL, downloaded (or imported from
# LAB_IMPORT_IMAGE) unless already there with that checksum. Fails on a
# checksum mismatch.
fetch() {
	local url="$1" sha="$2" path
	path="$LAB_CACHE_DIR/downloads/${url##*/}"
	mkdir -p "${path%/*}"
	if [[ -f "$path" ]] && echo "$sha  $path" | sha256sum -c --quiet >/dev/null 2>&1; then
		echo "$path"
		return
	fi
	if [[ -n "$LAB_IMPORT_IMAGE" ]] && echo "$sha  $LAB_IMPORT_IMAGE" | sha256sum -c --quiet >/dev/null 2>&1; then
		info "importing $LAB_IMPORT_IMAGE" >&2
		cp "$LAB_IMPORT_IMAGE" "$path.part" && mv "$path.part" "$path"
		echo "$path"
		return
	fi
	[[ -z "$LAB_IMPORT_IMAGE" ]] || info "LAB_IMPORT_IMAGE does not match the image's SHA-256: downloading" >&2
	info "downloading $url" >&2
	curl -fL --progress-bar -o "$path.part" "$url" >&2 || die "cannot download $url"
	echo "$sha  $path.part" | sha256sum -c --quiet >/dev/null 2>&1 || {
		rm -f "$path.part"
		die "checksum mismatch for $url (expected $sha)"
	}
	mv "$path.part" "$path"
	echo "$path"
}

# --- Lab state ----------------------------------------------------------------

env_file() { echo "$LAB_STATE_DIR/env.json"; }

# env_get FILTER: a value from env.json.
env_get() { jq -r "$1" "$(env_file)"; }

# env_set PATH VALUE: sets a string in env.json (PATH as in jq, e.g. .a.b).
env_set() {
	local tmp
	tmp="$(mktemp "$LAB_STATE_DIR/env.XXXXXX")"
	jq --arg v "$2" "$1 = \$v" "$(env_file)" >"$tmp" && mv "$tmp" "$(env_file)"
}

# secret NAME: a random secret the lab keeps in its state, made once.
secret() {
	local file="$LAB_STATE_DIR/$1"
	[[ -f "$file" ]] || (umask 077 && openssl rand -hex 16 >"$file")
	cat "$file"
}
