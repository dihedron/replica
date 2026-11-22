# shellcheck shell=bash
# The lab VM, "lxd", and the LXD that runs inside it: every server of the lab
# is a container of that inner LXD, on one of three region bridges
# (.specs/veho-lab.md §2). Copied from ligo 9f11da9 (test/lab/lib/inner.sh):
# three region bridges and a profile per region instead of one bridge;
# containers only.

# lxd_vm_cloud_init: the lab VM's cloud-init user data: the inner LXD, with
# refreshes held so that its version cannot change during a session, and the
# tools of the WAN (tc is in iproute2).
lxd_vm_cloud_init() {
	cat <<YAML
#cloud-config
package_update: true
packages: [nftables, jq]
snap:
  commands:
    - snap install lxd --channel=$LAB_INNER_LXD_CHANNEL
    - snap refresh --hold lxd
    - snap refresh --hold
YAML
}

# inner_preseed: the inner LXD's configuration: a pool on the VM's disk, a
# bridge per region and a profile per region. The bridges do not NAT: LXD would
# masquerade the traffic between regions too, and the WAN classifies it by its
# source address; wan_nat masquerades only what leaves the lab VM.
inner_preseed() {
	local x
	echo "storage_pools:"
	echo "  - name: default"
	echo "    driver: dir"
	echo "networks:"
	for x in "${REGIONS[@]}"; do
		cat <<YAML
  - name: region-$x
    type: bridge
    config:
      ipv4.address: $(region_gateway "$x")/24
      ipv4.nat: "false"
      ipv6.address: none
      dns.domain: $x.veho.lab
YAML
	done
	echo "profiles:"
	echo "  - name: default"
	echo "    devices:"
	echo "      root: {type: disk, path: /, pool: default}"
	for x in "${REGIONS[@]}"; do
		cat <<YAML
  - name: region-$x
    devices:
      root: {type: disk, path: /, pool: default}
      eth0: {type: nic, network: region-$x, name: eth0}
YAML
	done
}

# inner ARGS: lxc on the inner LXD.
inner() { lxd exec lxd -- lxc "$@"; }

# inner_exec NAME ARGS: runs ARGS in an inner instance, as root.
inner_exec() {
	local name="$1"
	shift
	lxd exec lxd -- lxc exec "$name" -- "$@"
}

# inner_push FILE NAME PATH MODE: copies a file of the lab host into an inner
# instance, owned by root.
inner_push() {
	local tmp="/root/push.$$"
	lxd file push "$1" "lxd$tmp" --mode 0600 >/dev/null
	inner file push "$tmp" "$2$3" --uid 0 --gid 0 --mode "$4" >/dev/null
	lxd exec lxd -- rm -f "$tmp"
}

# inner_write NAME PATH MODE: writes stdin to PATH in an inner instance.
inner_write() {
	inner_exec "$1" sh -c "umask 077 && cat >'$2.new' && chmod $3 '$2.new' && mv '$2.new' '$2'"
}

# inner_image: the lab image under the local alias lab-container of the
# inner LXD, copied once: creating an instance from the "ubuntu:" remote makes
# the lxc client query it first, and that query has hung on ligo's lab.
inner_image() {
	inner image info lab-container >/dev/null 2>&1 && return
	lxd exec lxd -- timeout 900 lxc image copy "$LAB_IMAGE" local: --alias lab-container </dev/null >/dev/null ||
		lxd exec lxd -- timeout 900 lxc image copy "$LAB_IMAGE" local: --alias lab-container </dev/null >/dev/null ||
		die "cannot copy $LAB_IMAGE into the inner LXD"
	info "inner LXD: $LAB_IMAGE cached as lab-container"
}

# inner_create NAME MEMORY [ARGS]: creates a container of the inner LXD in
# NAME's region, with its static address, unless it exists; with an empty
# stdin (lxc init reads YAML from a stdin that is not a terminal), a timeout
# and one retry.
inner_create() {
	local name="$1" memory="$2" region
	shift 2
	inner_exists "$name" && return
	region="${name#*-}" && region="${region:0:1}"
	# shellcheck disable=SC2054 # eth0,ipv4.address=... is one lxc argument
	local args=(init lab-container "$name" -p "region-$region" -c limits.memory="$memory" -c boot.autostart=true
		-d eth0,ipv4.address="$(inner_ip "$name")" "$@")
	lxd exec lxd -- timeout 300 lxc "${args[@]}" </dev/null >/dev/null || {
		warn "creating $name timed out: retrying once"
		inner delete "$name" --force >/dev/null 2>&1 || true
		lxd exec lxd -- timeout 300 lxc "${args[@]}" </dev/null >/dev/null || die "cannot create $name"
	}
	info "container $name created in region $region ($(inner_ip "$name"))"
}

# inner_start NAME: starts an inner instance unless it is running; instances
# have boot.autostart, so one may start by itself meanwhile.
inner_start() {
	inner_running "$1" && return
	inner start "$1" >/dev/null 2>&1 || inner_running "$1" || die "cannot start $1"
}

# inner_exists NAME / inner_running NAME
inner_exists() { inner info "$1" >/dev/null 2>&1; }
inner_running() { [[ "$(inner list "$1" -c s -f csv 2>/dev/null)" == RUNNING ]]; }

# inner_wait NAME: waits until an inner instance answers and cloud-init is
# done.
inner_wait() {
	for _ in $(seq 1 120); do
		inner_exec "$1" true >/dev/null 2>&1 && break
		sleep 2
	done
	cloud_init_wait "$1" inner_exec "$1"
}

# inner_proxy NAME DEVICE PORT TARGET: publishes TARGET (a port of NAME) on
# the lab VM's lab address at PORT; nat mode keeps the client's address.
inner_proxy() {
	inner config device get "$1" "$2" type >/dev/null 2>&1 && return
	inner config device add "$1" "$2" proxy nat=true \
		listen="tcp:$(lxd_ip):$3" connect="tcp:$(inner_ip "$1"):$4" >/dev/null
}

# lxd_vm_up creates the lab VM and initializes its inner LXD.
lxd_vm_up() {
	section "Lab VM (lxd) and its inner LXD"
	if ! vm_exists lxd; then
		lxd init "$LAB_IMAGE" lxd --vm </dev/null \
			-c limits.cpu="$LAB_LXD_CPUS" -c limits.memory="$LAB_LXD_MEMORY" \
			-c cloud-init.user-data="$(lxd_vm_cloud_init)" \
			-d root,size="$LAB_LXD_DISK" \
			-d eth0,ipv4.address="$(lxd_ip)" >/dev/null
		info "VM lxd created from $LAB_IMAGE"
	else
		lxd_vm_check_size
	fi
	vm_running lxd || lxd start lxd
	info "waiting for cloud-init (inner LXD)..."
	vm_wait lxd
	if ! lxd exec lxd -- lxc network show region-a >/dev/null 2>&1; then
		inner_preseed | lxd exec lxd -- lxd init --preseed
		info "inner LXD initialized: bridges region-a, region-b, region-c ($LAB_REGION_PREFIX.{1,2,3}.0/24)"
	else
		info "inner LXD initialized"
	fi
	local x
	for x in "${REGIONS[@]}"; do
		[[ "$(inner network get "region-$x" ipv4.nat)" == false ]] || inner network set "region-$x" ipv4.nat false
	done
	wan_nat
	inner_image
}

# lxd_vm_check_size warns when the lab VM differs from the size settings.
lxd_vm_check_size() {
	local cpus memory
	cpus="$(lxd config get lxd limits.cpu)"
	memory="$(lxd config get lxd limits.memory)"
	[[ "$cpus" == "$LAB_LXD_CPUS" && "$memory" == "$LAB_LXD_MEMORY" ]] ||
		warn "VM lxd has $cpus vCPU(s) and $memory, the settings ask for $LAB_LXD_CPUS and $LAB_LXD_MEMORY: \"down\" then \"up\" to apply them"
}
