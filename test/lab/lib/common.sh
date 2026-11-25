# shellcheck shell=bash
# Common functions of the lab: settings, output and small helpers. Sourced by
# lab.sh, which sets LAB_DIR (test/lab) and REPO_DIR (the repository).
# Copied from ligo 9f11da9 (test/lab/lib/common.sh): no scenarios, the
# addresses of the three regions (.specs/veho-lab.md §2).

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

# env_set PATH JSON: sets a value in env.json (jq path, JSON value).
env_set() {
	local file="$LAB_STATE_DIR/env.json"
	[[ -f "$file" ]] || echo '{}' >"$file"
	jq --argjson v "$2" "$1 = \$v" "$file" >"$file.new" && mv "$file.new" "$file"
}

# env_get PATH: reads a value from env.json (raw).
env_get() { jq -r "$1" "$LAB_STATE_DIR/env.json"; }

# --- Output -------------------------------------------------------------------

if [[ -t 1 ]]; then
	_green=$'\e[32m' _yellow=$'\e[33m' _red=$'\e[31m' _bold=$'\e[1m' _reset=$'\e[0m'
else
	_green='' _yellow='' _red='' _bold='' _reset=''
fi

section() { printf '\n%s%s%s\n' "$_bold" "$*" "$_reset"; }
info() { printf '        %s\n' "$*"; }
die() {
	printf '%serror:%s %s\n' "$_red" "$_reset" "$*" >&2
	exit 1
}

# Check results, counted by the preflight and the selftest.
CHECK_FAILS=0
CHECK_WARNS=0
ok() { printf '  %sok%s    %s\n' "$_green" "$_reset" "$*"; }
warn() {
	printf '  %swarn%s  %s\n' "$_yellow" "$_reset" "$*"
	CHECK_WARNS=$((CHECK_WARNS + 1))
}
fail() {
	printf '  %sfail%s  %s\n' "$_red" "$_reset" "$*"
	CHECK_FAILS=$((CHECK_FAILS + 1))
}

# confirm QUESTION: true when the user (or LAB_ASSUME_YES) says yes.
confirm() {
	[[ "$LAB_ASSUME_YES" == yes ]] && return 0
	local answer
	read -r -p "$1 [y/N] " answer
	[[ "$answer" == [yY] || "$answer" == [yY][eE][sS] ]]
}

# --- Regions and addresses ----------------------------------------------------

# The regions, lower case; a region's number is its position (a=1).
REGIONS=(a b c)

# region_check X: fails unless X is a region.
region_check() { [[ " ${REGIONS[*]} " == *" ${1:-} "* ]] || die "no region \"${1:-}\": a, b or c"; }

# region_num X: 1, 2 or 3.
region_num() {
	case "$1" in a) echo 1 ;; b) echo 2 ;; c) echo 3 ;; *) die "no region \"$1\"" ;; esac
}

# region_subnet X: the region's /24.
region_subnet() { printf '%s.%s.0/24' "$LAB_REGION_PREFIX" "$(region_num "$1")"; }
# region_gateway X: the lab VM's address on the region's bridge.
region_gateway() { printf '%s.%s.1' "$LAB_REGION_PREFIX" "$(region_num "$1")"; }

# prefix24 CIDR: the first three octets of a /24.
prefix24() { printf '%s' "${1%.*}"; }
# lab_gateway: the outer bridge's address on the lab subnet.
lab_gateway() { printf '%s.1' "$(prefix24 "$LAB_SUBNET")"; }
# lxd_ip: the lab VM's address on the lab subnet.
lxd_ip() { printf '%s.%s' "$(prefix24 "$LAB_SUBNET")" "$LAB_LXD_HOST"; }

# inner_ip NAME: an inner instance's address: db-X .10, s3-X .20, veho-X1 .31,
# veho-X2 .32, in region X's subnet.
inner_ip() {
	local name="$1" region host
	case "$name" in
	db-?) region="${name#db-}" host=10 ;;
	s3-?) region="${name#s3-}" host=20 ;;
	nexus-?) region="${name#nexus-}" host=40 ;;
	veho-??) region="${name:5:1}" host="3${name:6:1}" ;;
	*) die "no address for $name" ;;
	esac
	printf '%s.%s.%s' "$LAB_REGION_PREFIX" "$(region_num "$region")" "$host"
}

# published_port BASE X: the port on the lab VM's address that publishes a
# region's service: BASE plus the region's number.
published_port() { echo $(($1 + $(region_num "$2"))); }

# --- LXD ----------------------------------------------------------------------

# lxd: lxc in the lab's project.
lxd() { lxc --project "$LAB_PROJECT" "$@"; }

# lxd_once SECONDS ARGS: lxc in the lab's project, with an empty stdin (some
# lxc commands read YAML from stdin when it is not a terminal, and wait on a
# pipe that never closes), retried once after a timeout.
lxd_once() {
	local seconds="$1"
	shift
	timeout "$seconds" lxc --project "$LAB_PROJECT" "$@" </dev/null && return 0
	local status=$?
	((status == 124)) || return "$status"
	warn "lxc $* timed out after ${seconds}s: retrying once"
	timeout "$seconds" lxc --project "$LAB_PROJECT" "$@" </dev/null
}

# lxd_reachable: true when this session can talk to the LXD daemon.
lxd_reachable() { lxc query /1.0 >/dev/null 2>&1; }

# ensure_lxd_access ARGS: when this session cannot reach LXD but the user is
# in the lxd group (joined after this session started), runs lab.sh ARGS
# again through "sg lxd".
ensure_lxd_access() {
	lxd_reachable || [[ -n "${LAB_SG_DONE:-}" ]] || ! command -v lxc >/dev/null && return 0
	if getent group lxd | cut -d: -f4 | tr ',' '\n' | grep -x "$(id -un)" >/dev/null; then
		export LAB_SG_DONE=1
		exec sg lxd -c "$(printf '%q ' "$LAB_DIR/lab.sh" "$@")"
	fi
}

# password NAME: a secret of the state directory, created on first use.
password() {
	local file="$LAB_STATE_DIR/passwords/$1"
	if [[ ! -f "$file" ]]; then
		mkdir -p "${file%/*}" && chmod 0700 "${file%/*}"
		(umask 077 && openssl rand -hex 24 >"$file")
	fi
	cat "$file"
}
