#!/usr/bin/env bash
# veho's DevStack lab: one libvirt VM with Keystone, Glance and Swift, for
# the OpenStack drivers' tests. See .specs/veho-openstack.md §6 and
# "lab.sh help". Self-contained: it needs nothing from any other lab.
set -euo pipefail

LAB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "$LAB_DIR/../.." && pwd)"

for lib in common vm devstack snapshot; do
	# shellcheck source=/dev/null
	source "$LAB_DIR/lib/$lib.sh"
done

usage() {
	cat <<USAGE
usage: lab.sh <command> [options]

commands:
  preflight            check that this machine can run the lab
  up                   build the lab (or complete a partial one), then take
                       the baseline snapshot when there is none
  down                 destroy the lab: VM, network, storage pool and state,
                       after confirmation (the download cache stays)
  status               the VM, the versions and DevStack
  ssh [command]        open a shell, or run a command, on the VM
  logs [unit]          stack.sh's log, or a systemd unit's journal
  snapshot             save the running lab as its baseline
  reset                return the lab to its baseline, in minutes
  stop                 save the VM, memory included, and stop it
  start                resume the VM saved by stop
  env                  the tests' environment, as export lines:
                       eval "\$(test/devstack/lab.sh env)"
  test [-run REGEX] [pkg...]
                       go test -tags devstack (default ./pkg/plugin/...)
                       with that environment
  help                 show this help

Settings are in lab.env, overridden by lab.local.env and by the environment.
USAGE
}

# --- Preflight ----------------------------------------------------------------

cmd_preflight() {
	section "Preflight"
	local failed=0 tool
	[[ "$(uname -s)/$(uname -m)" == Linux/x86_64 ]] && ok "Linux on x86-64" || {
		info "FAIL: the lab needs Linux on x86-64"
		failed=1
	}
	[[ -w /dev/kvm ]] && ok "/dev/kvm" || {
		info "FAIL: /dev/kvm is missing or not writable (KVM, and the kvm group)"
		failed=1
	}
	for tool in virsh virt-install qemu-img jq curl git ssh ssh-keygen openssl sha256sum; do
		have "$tool" || {
			info "FAIL: $tool is missing"
			failed=1
		}
	done
	have cloud-localds || have xorriso || have genisoimage || {
		info "FAIL: one of cloud-localds, xorriso or genisoimage is needed"
		failed=1
	}
	((failed)) || ok "tools"
	if virsh -q -c qemu:///system version >/dev/null 2>&1; then
		ok "libvirt (qemu:///system)"
	else
		info "FAIL: cannot connect to qemu:///system (the libvirt group?)"
		failed=1
	fi
	local avail_gib free_gib dir="$LAB_POOL_DIR"
	avail_gib=$(($(awk '/^MemAvailable:/ { print $2 }' /proc/meminfo) / 1024 / 1024))
	if ((avail_gib >= LAB_DEVSTACK_RAM_GIB)) || vsh dominfo "$(vm_domain)" >/dev/null 2>&1; then
		ok "memory: $avail_gib GiB available, the VM takes $LAB_DEVSTACK_RAM_GIB"
	else
		info "FAIL: $avail_gib GiB of memory available, the VM takes $LAB_DEVSTACK_RAM_GIB"
		failed=1
	fi
	while [[ ! -d "$dir" ]]; do dir="$(dirname "$dir")"; done
	free_gib=$(($(df --output=avail -k "$dir" | tail -n1) / 1024 / 1024))
	if ((free_gib >= LAB_DEVSTACK_DISK_GIB / 2)); then
		ok "disk: $free_gib GiB free under $dir (the VM's disk grows up to $LAB_DEVSTACK_DISK_GIB)"
	else
		info "FAIL: $free_gib GiB free under $dir"
		failed=1
	fi
	if ip -4 route | awk '{ print $1 }' | grep -qx "$LAB_SUBNET" && ! vsh net-info "$LAB_NETWORK" >/dev/null 2>&1; then
		info "FAIL: $LAB_SUBNET is already routed on this host: set LAB_SUBNET in lab.local.env"
		failed=1
	else
		ok "network $LAB_SUBNET"
	fi
	((failed == 0)) || die "the preflight failed"
}

# --- Commands -----------------------------------------------------------------

require_state() { [[ -f "$(env_file)" ]] || die "no lab here: run lab.sh up first"; }

# resolve_versions: records the versions in env.json, unless a previous
# (possibly partial) "up" did; "down" forgets them.
resolve_versions() {
	[[ -f "$(env_file)" ]] && return
	section "Versions"
	local branch url sha
	branch="$(resolve_devstack_branch)"
	url="$(ubuntu_image_url)"
	sha="${LAB_UBUNTU_IMAGE_SHA256:-$(published_sha256 "$url")}" || die "no published checksum for $url: set LAB_UBUNTU_IMAGE_SHA256"
	jq -n --arg branch "$branch" --arg release "$LAB_UBUNTU_RELEASE" --arg url "$url" --arg sha "$sha" \
		--arg ip "$(vm_ip)" --arg subnet "$LAB_SUBNET" \
		'{versions: {devstack_branch: $branch}, image: {release: $release, url: $url, sha256: $sha}, vm: {ip: $ip}, network: {subnet: $subnet}}' >"$(env_file)"
	info "DevStack $branch, Ubuntu $LAB_UBUNTU_RELEASE"
}

cmd_up() {
	cmd_preflight
	mkdir -p "$LAB_STATE_DIR"
	ensure_ssh_key
	resolve_versions
	if vm_saved; then
		section "Start"
		lab_start
	fi
	section "VM"
	ensure_pool
	ensure_base_image
	ensure_network
	ensure_vm
	wait_for_vm
	section "DevStack"
	devstack_up
	devstack_configure
	devstack_smoke
	if ! snapshot_exists; then
		section "Snapshot"
		LAB_ASSUME_YES=1 lab_snapshot
	fi
	section "Done"
	info "the lab is up: lab.sh status, lab.sh test, lab.sh reset"
}

cmd_down() {
	confirm "Destroy the lab (VM, network $LAB_NETWORK, storage pool $LAB_POOL and $LAB_STATE_DIR)?" || {
		info "nothing destroyed"
		return 1
	}
	destroy_all
	rm -rf "$LAB_STATE_DIR"
	info "the lab is gone (the download cache in $LAB_CACHE_DIR stays)"
}

cmd_status() {
	require_state
	section "Versions"
	info "DevStack: $(env_get '.versions.devstack_branch') $(env_get '.versions.devstack_commit // "" | .[0:12]')"
	info "image:    Ubuntu $(env_get .image.release)"
	section "VM"
	local state
	state="$(vsh domstate "$(vm_domain)" 2>/dev/null || echo absent)"
	vm_saved && state="saved (lab.sh start)"
	info "$(vm_domain)  $(vm_ip)  $state"
	section "Snapshot"
	snapshot_exists && info "taken $(env_get '.snapshot.taken')" || info "none (lab.sh snapshot)"
	section "DevStack"
	if [[ "$state" == running ]] && devstack_stacked; then
		devstack_smoke
	else
		info "not running"
	fi
}

# cmd_env: the tests' environment. The secrets stay in the state directory;
# the lines name them, so the output is safe to show.
cmd_env() {
	require_state
	[[ -n "$(env_get '.openstack.app_cred_id // ""')" ]] || die "DevStack is not configured: run lab.sh up"
	cat <<ENV
export VEHO_DEVSTACK_AUTH_URL=$(env_get .openstack.auth_url)
export VEHO_DEVSTACK_REGION=$(env_get .openstack.region)
export VEHO_DEVSTACK_CA_FILE=$(ca_file)
export VEHO_DEVSTACK_ALLOW_CIDR=$LAB_SUBNET
export VEHO_DEVSTACK_HOST_IP=$(lab_ip 1)
export VEHO_DEVSTACK_APP_CRED_ID=$(env_get .openstack.app_cred_id)
export VEHO_DEVSTACK_APP_CRED_SECRET_FILE=$LAB_STATE_DIR/app-cred-secret
export VEHO_DEVSTACK_USERNAME=$(env_get .openstack.user)
export VEHO_DEVSTACK_PASSWORD_FILE=$LAB_STATE_DIR/test-password
export VEHO_DEVSTACK_PROJECT=$(env_get .openstack.project)
ENV
}

cmd_test() {
	local args=() pkgs=()
	while (($#)); do
		case "$1" in
		-run)
			[[ $# -ge 2 ]] || die "usage: lab.sh test [-run REGEX] [pkg...]"
			args+=(-run "$2")
			shift 2
			;;
		*)
			pkgs+=("$1")
			shift
			;;
		esac
	done
	((${#pkgs[@]})) || pkgs=(./pkg/plugin/...)
	eval "$(cmd_env)"
	cd "$REPO_DIR"
	go test -tags devstack -count=1 -p 1 -timeout 60m "${args[@]}" "${pkgs[@]}"
}

main() {
	local command="${1:-help}"
	shift || true
	load_settings
	load_ssh
	case "$command" in
	preflight) cmd_preflight ;;
	up) cmd_up ;;
	down) cmd_down ;;
	status) cmd_status ;;
	ssh)
		require_state
		require_started
		exec ssh -t "${SSH_OPTS[@]}" "$LAB_USER@$(vm_ip)" "$@"
		;;
	logs)
		require_state
		require_started
		if [[ -n "${1:-}" ]]; then
			vm_ssh "sudo journalctl --no-pager -n 300 -u $(printf '%q' "$1")"
		elif vm_ssh "test -e $DEVSTACK_LOG"; then
			vm_ssh "sudo tail -n 300 $DEVSTACK_LOG"
		else
			vm_ssh "sudo tail -n 300 /var/log/cloud-init-output.log"
		fi
		;;
	snapshot)
		require_state
		require_started
		lab_snapshot
		;;
	reset)
		require_state
		lab_reset
		;;
	stop)
		require_state
		lab_stop
		;;
	start)
		require_state
		lab_start
		;;
	env) cmd_env ;;
	test)
		require_state
		require_started
		cmd_test "$@"
		;;
	help | -h | --help) usage ;;
	*)
		usage >&2
		exit 2
		;;
	esac
}

main "$@"
