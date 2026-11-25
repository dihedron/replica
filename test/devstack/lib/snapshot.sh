# shellcheck shell=bash
# The baseline snapshot, and stop and start. Snapshots are internal to the
# VM's disk and include its memory, so a reset resumes DevStack where it was
# instead of booting it (DevStack is not made to survive a cold boot).

readonly LAB_SNAPSHOT=lab-base

snapshot_exists() { vsh snapshot-info "$(vm_domain)" "$LAB_SNAPSHOT" >/dev/null 2>&1; }

vm_saved() { [[ "$(vsh domstate --reason "$(vm_domain)" 2>/dev/null)" == "shut off (saved)" ]]; }

require_started() { ! vm_saved || die "the lab is stopped: run lab.sh start first"; }

# set_clock: the VM's clock from the lab host's; NTP takes over from there.
set_clock() {
	local deadline=$((SECONDS + 120))
	until vm_ssh "sudo date -u -s @$(date +%s.%N) >/dev/null" 2>/dev/null; do
		((SECONDS < deadline)) || die "the VM does not answer over SSH after the revert"
		sleep 2
	done
}

# lab_snapshot: saves the running VM as the lab's baseline.
lab_snapshot() {
	[[ "$(vsh domstate "$(vm_domain)" 2>/dev/null)" == running ]] || die "the VM is not running: run lab.sh up first"
	if snapshot_exists; then
		confirm "Replace the lab's snapshot, taken $(env_get '.snapshot.taken // "at an unknown time"')?" || {
			info "snapshot kept"
			return 1
		}
		vsh snapshot-delete "$(vm_domain)" "$LAB_SNAPSHOT" >/dev/null
	fi
	info "snapshotting the VM"
	vsh snapshot-create-as "$(vm_domain)" "$LAB_SNAPSHOT" --description "veho DevStack lab baseline" >/dev/null
	env_set .snapshot.taken "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
	info "snapshot taken: lab.sh reset returns the lab to this state"
}

# lab_reset: returns the VM to the snapshot (a saved state is discarded),
# fixes its clock, and checks DevStack.
lab_reset() {
	snapshot_exists || die "no snapshot: run lab.sh up (or lab.sh snapshot)"
	if vm_saved; then
		info "discarding the saved state"
		vsh managedsave-remove "$(vm_domain)" >/dev/null
	fi
	info "reverting the VM"
	vsh snapshot-revert "$(vm_domain)" "$LAB_SNAPSHOT" --running >/dev/null
	set_clock
	devstack_smoke
	info "the lab is back to the snapshot taken $(env_get '.snapshot.taken')"
}

readonly LIBVIRT_SAVE_DIR=/var/lib/libvirt/qemu/save

# lab_stop: saves the running VM, memory included, after checking that
# libvirt's save directory has room for it plus 10%.
lab_stop() {
	if [[ "$(vsh domstate "$(vm_domain)" 2>/dev/null)" != running ]]; then
		vm_saved && info "the lab is already stopped: lab.sh start resumes it" || info "the VM is not running: nothing to save"
		return
	fi
	local need_kib avail_kib
	need_kib="$(vsh dominfo "$(vm_domain)" | awk '/^Used memory:/ { print $3 }')"
	need_kib=$((need_kib * 110 / 100))
	avail_kib="$(df --output=avail -k "$(dirname "$LIBVIRT_SAVE_DIR")" | tail -n1)"
	((avail_kib >= need_kib)) ||
		die "saving the VM needs $((need_kib / 1024 / 1024)) GiB under $LIBVIRT_SAVE_DIR, $((avail_kib / 1024 / 1024)) GiB are free"
	info "saving the VM"
	vsh managedsave "$(vm_domain)" >/dev/null || die "cannot save the VM (lab.sh status)"
	info "the lab is stopped, as it was: lab.sh start resumes it"
}

# lab_start: resumes the saved VM, fixes its clock, and checks DevStack.
lab_start() {
	local state
	state="$(vsh domstate "$(vm_domain)" 2>/dev/null)" || die "the VM does not exist: run lab.sh up"
	if ! vm_saved; then
		[[ "$state" == running ]] || die "the VM is $state without a saved state: run lab.sh reset"
		info "the lab is already running"
		return
	fi
	info "resuming the VM"
	vsh start "$(vm_domain)" >/dev/null || die "cannot resume the VM (lab.sh status)"
	set_clock
	devstack_smoke
	info "the lab is running again, as it was stopped"
}
