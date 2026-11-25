# shellcheck shell=bash
# The lab's libvirt resources: storage pool, base image, network and the VM.

readonly LAB_VM=devstack
readonly LAB_USER=lab

vsh() { virsh -q -c qemu:///system "$@"; }

vm_ip() { lab_ip "$LAB_DEVSTACK_HOST"; }
vm_domain() { echo "$LAB_NETWORK"; }
vm_mac() { printf '52:54:00:fd:00:%02x' "$LAB_DEVSTACK_HOST"; }

# --- SSH ----------------------------------------------------------------------

ssh_key() { echo "$LAB_STATE_DIR/id_ed25519"; }

SSH_OPTS=()
load_ssh() {
	SSH_OPTS=(-i "$(ssh_key)" -o "UserKnownHostsFile=$LAB_STATE_DIR/known_hosts"
		-o StrictHostKeyChecking=accept-new -o ConnectTimeout=5 -o BatchMode=yes
		-o LogLevel=ERROR -o ServerAliveInterval=15)
}

# vm_ssh [COMMAND...]: runs COMMAND (a shell string) on the VM as the lab user.
vm_ssh() { ssh "${SSH_OPTS[@]}" "$LAB_USER@$(vm_ip)" "$@"; }

ensure_ssh_key() {
	[[ -f "$(ssh_key)" ]] || ssh-keygen -q -t ed25519 -N '' -C "veho-devstack-lab" -f "$(ssh_key)"
}

# --- Storage ------------------------------------------------------------------

ensure_pool() {
	if ! vsh pool-info "$LAB_POOL" >/dev/null 2>&1; then
		info "creating storage pool $LAB_POOL in $LAB_POOL_DIR"
		vsh pool-define-as "$LAB_POOL" dir --target "$LAB_POOL_DIR" >/dev/null
		vsh pool-build "$LAB_POOL" >/dev/null
		vsh pool-autostart "$LAB_POOL" >/dev/null
	fi
	[[ "$(vsh pool-info "$LAB_POOL" | awk '/^State:/ { print $2 }')" == running ]] || vsh pool-start "$LAB_POOL" >/dev/null
}

# upload_volume NAME FILE: uploads FILE as volume NAME, unless it exists.
upload_volume() {
	vsh vol-info --pool "$LAB_POOL" "$1" >/dev/null 2>&1 && return 0
	info "uploading $1"
	vsh vol-create-as "$LAB_POOL" "$1" "$(stat -c %s "$2")" --format raw >/dev/null
	vsh vol-upload --pool "$LAB_POOL" "$1" "$2"
	vsh pool-refresh "$LAB_POOL" >/dev/null
}

# base_volume: the base image's volume, named after the start of its
# checksum, so a newer cloud image never alters a VM built on the previous.
base_volume() { echo "base-ubuntu-$(env_get '.image.sha256' | cut -c1-12).qcow2"; }

ensure_base_image() {
	upload_volume "$(base_volume)" "$(fetch "$(env_get .image.url)" "$(env_get .image.sha256)")"
}

# --- Network ------------------------------------------------------------------

ensure_network() {
	if ! vsh net-info "$LAB_NETWORK" >/dev/null 2>&1; then
		info "creating network $LAB_NETWORK ($LAB_SUBNET)"
		local xml="$LAB_STATE_DIR/network.xml"
		cat >"$xml" <<XML
<network>
  <name>$LAB_NETWORK</name>
  <forward mode='nat'/>
  <bridge stp='on' delay='0'/>
  <domain name='lab' localOnly='yes'/>
  <dns><host ip='$(vm_ip)'><hostname>devstack.lab</hostname></host></dns>
  <ip address='$(lab_ip 1)' netmask='255.255.255.0'>
    <dhcp>
      <range start='$(lab_ip 200)' end='$(lab_ip 250)'/>
      <host mac='$(vm_mac)' name='devstack' ip='$(vm_ip)'/>
    </dhcp>
  </ip>
</network>
XML
		vsh net-define "$xml" >/dev/null
		vsh net-autostart "$LAB_NETWORK" >/dev/null
	fi
	[[ "$(vsh net-info "$LAB_NETWORK" | awk '/^Active:/ { print $2 }')" == yes ]] || vsh net-start "$LAB_NETWORK" >/dev/null
}

# --- The VM -------------------------------------------------------------------

# seed_image: builds the cloud-init seed and prints its path.
seed_image() {
	local dir="$LAB_STATE_DIR/seed"
	mkdir -p "$dir"
	cat >"$dir/meta-data" <<META
instance-id: devstack-$(date +%s)
local-hostname: devstack
META
	cat >"$dir/user-data" <<USER
#cloud-config
hostname: devstack
fqdn: devstack.lab
manage_etc_hosts: true
ssh_pwauth: false
users:
  - name: $LAB_USER
    gecos: veho DevStack lab
    shell: /bin/bash
    sudo: "ALL=(ALL) NOPASSWD:ALL"
    lock_passwd: true
    ssh_authorized_keys:
      - $(cat "$(ssh_key).pub")
USER
	local iso="$dir/seed.iso"
	if have cloud-localds; then
		cloud-localds "$iso" "$dir/user-data" "$dir/meta-data"
	elif have xorriso; then
		xorriso -as genisoimage -quiet -output "$iso" -volid cidata -joliet -rock "$dir/user-data" "$dir/meta-data" 2>/dev/null
	else
		genisoimage -quiet -output "$iso" -volid cidata -joliet -rock "$dir/user-data" "$dir/meta-data"
	fi
	echo "$iso"
}

# vm_osinfo: the most precise OS name this host's libvirt knows.
vm_osinfo() {
	local known name
	known=" $(virt-install --osinfo list 2>/dev/null | tr ',' '\n' | tr -d ' ' | tr '\n' ' ') "
	for name in "ubuntu$LAB_UBUNTU_RELEASE" linux2024 linux2022; do
		if [[ "$known" == *" $name "* ]]; then
			echo "$name"
			return
		fi
	done
	echo generic
}

# ensure_vm: creates and starts the VM, or starts it if it is shut off.
ensure_vm() {
	local domain
	domain="$(vm_domain)"
	if vsh dominfo "$domain" >/dev/null 2>&1; then
		[[ "$(vsh domstate "$domain")" == running ]] || vsh start "$domain" >/dev/null
		return
	fi
	info "creating VM $domain ($(vm_ip))"
	local disk=devstack.qcow2 seed=devstack-seed.iso
	vsh vol-delete --pool "$LAB_POOL" "$disk" >/dev/null 2>&1 || true
	vsh vol-delete --pool "$LAB_POOL" "$seed" >/dev/null 2>&1 || true
	vsh vol-create-as "$LAB_POOL" "$disk" "${LAB_DEVSTACK_DISK_GIB}G" --format qcow2 \
		--backing-vol "$(base_volume)" --backing-vol-format qcow2 >/dev/null
	upload_volume "$seed" "$(seed_image)"
	virt-install --connect qemu:///system --name "$domain" \
		--memory $((LAB_DEVSTACK_RAM_GIB * 1024)) --vcpus "$LAB_DEVSTACK_VCPUS" \
		--cpu host-passthrough --osinfo "$(vm_osinfo)" \
		--disk "vol=$LAB_POOL/$disk,bus=virtio" \
		--disk "vol=$LAB_POOL/$seed,device=cdrom" \
		--network "network=$LAB_NETWORK,mac=$(vm_mac),model=virtio" \
		--import --graphics none --console pty,target_type=serial --noautoconsole >/dev/null
}

# wait_for_vm: waits until the VM answers over SSH and cloud-init is done.
wait_for_vm() {
	local deadline=$((SECONDS + 900)) status=0
	until vm_ssh true 2>/dev/null; do
		((SECONDS < deadline)) || die "the VM does not answer over SSH at $(vm_ip) (see: virsh console $(vm_domain))"
		sleep 5
	done
	vm_ssh sudo cloud-init status --wait >/dev/null 2>&1 || status=$?
	case "$status" in
	0) info "the VM is up" ;;
	2) info "the VM is up (cloud-init reported recoverable errors: lab.sh logs)" ;;
	*) die "cloud-init failed on the VM (lab.sh logs)" ;;
	esac
}

# --- Teardown -----------------------------------------------------------------

destroy_all() {
	local domain volume
	domain="$(vm_domain)"
	if vsh dominfo "$domain" >/dev/null 2>&1; then
		info "destroying VM $domain"
		vsh destroy "$domain" >/dev/null 2>&1 || true
		vsh managedsave-remove "$domain" >/dev/null 2>&1 || true
		vsh undefine "$domain" --remove-all-storage --snapshots-metadata >/dev/null 2>&1 ||
			vsh undefine "$domain" --snapshots-metadata >/dev/null
	fi
	if vsh pool-info "$LAB_POOL" >/dev/null 2>&1; then
		info "destroying storage pool $LAB_POOL"
		vsh pool-start "$LAB_POOL" >/dev/null 2>&1 || true
		while read -r volume _; do
			[[ -n "$volume" ]] && vsh vol-delete --pool "$LAB_POOL" "$volume" >/dev/null
		done < <(vsh vol-list "$LAB_POOL")
		vsh pool-destroy "$LAB_POOL" >/dev/null 2>&1 || true
		vsh pool-delete "$LAB_POOL" >/dev/null 2>&1 ||
			info "warning: $LAB_POOL_DIR could not be removed: remove it by hand (sudo rm -r $LAB_POOL_DIR)"
		vsh pool-undefine "$LAB_POOL" >/dev/null
	fi
	if vsh net-info "$LAB_NETWORK" >/dev/null 2>&1; then
		info "destroying network $LAB_NETWORK"
		vsh net-destroy "$LAB_NETWORK" >/dev/null 2>&1 || true
		vsh net-undefine "$LAB_NETWORK" >/dev/null
	fi
}
