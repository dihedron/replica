#!/usr/bin/env bash
# The veho lab: three regions in one LXD VM, for the integration and
# acceptance tests. See .specs/veho-lab.md and "lab.sh help".
set -euo pipefail

LAB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "$LAB_DIR/../.." && pwd)"

for lib in common preflight lxd inner pki fetch wan db s3 nodes campaign snapshot selftest; do
	# shellcheck source=/dev/null
	source "$LAB_DIR/lib/$lib.sh"
done

usage() {
	cat <<USAGE
usage: lab.sh <command> [options]

commands:
  preflight                 check that this machine can run the lab
  up                        build the lab (or complete a partial one), then
                            take the baseline snapshot when there is none
  down                      destroy the lab: VM, snapshots, network, project,
                            storage pool and state, after confirmation
  status                    the instances, the Patroni cluster, the WAN and
                            what was recorded
  env                       the tests' environment, as export lines:
                            eval "\$(test/lab/lab.sh env)"
  test [-run REGEX] [pkg...]
                            go test -tags integration (default ./...) with
                            that environment
  selftest                  check the lab (LAB-4 to LAB-8); switches the
                            primary over
  wan show                  the links and partitions between the regions
  wan set PROFILE [--jitter MS|N%] [--loss PCT]
                            apply a profile of lab.env (none, fiber1, fiber2,
                            detour, rtt20, rtt30, rtt40)
  wan set --link PAIR=DELAY[/JITTER[/LOSS]] ...
                            change single links (a-b, a-c, b-c; one-way ms)
  wan partition X|X-Y       cut region X from the others, or one pair
  wan heal                  remove every partition
  psql [--region X] [args]  psql as veho_migrator over TLS: the primary, or
                            the member of region X
  schema-reset              drop and recreate the schema veho, empty, for a
                            migration edited in place; the tests migrate it
  nodes up|down             create or remove the veho nodes veho-a1 to
                            veho-c2 (not in the baseline snapshot)
  nodes deploy [--workflow-version V] [NAME...]
                            build veho and the fault proxy, migrate, set the
                            KEK and restart the nodes (or NAMEs) one at a
                            time; V builds veho with that workflow version
  acceptance [-run R] [--no-deploy]
                            nodes deploy, then the daytime acceptance suite
                            (go test -tags acceptance ./test/acceptance)
  campaign soak [--duration D] [--jobs N] | wan24 [--hours H] | long
                            the long campaigns (AC-17, AC-18, AC-1 at 4 GiB
                            and AC-8 for 30 minutes), overnight; logs in
                            .evidence/
  node start|stop|restart NAME
  node kill NAME [--stay-down]
                            SIGKILL veho; --stay-down keeps it down until
                            node start
  node pause|resume NAME    SIGSTOP / SIGCONT veho
  db kill|start X           force-stop / start db-X (Patroni member)
  db-certs                  issue the members' certificates again and
                            reload PostgreSQL (no restart)
  fault X status CODE [--for D] | corrupt [--count N] | delay D [--bucket B]
  fault X clear|show        the fault proxy in front of region X's Garage
  patronictl [args]         patronictl in db-b
  etcdctl [args]            etcdctl in db-b, against the three members
  garage X [args]           the garage CLI of region X
  snapshot                  save the VM as the lab's baseline
  reset                     return to the baseline: empty database, empty
                            buckets, default WAN
  stop / start              stop the VM cleanly / start it as it was
  ssh NAME [command]        a root shell, or a command, in the lab VM (lxd)
                            or a container (db-a, s3-b, ...)
  logs NAME [unit]          its cloud-init log, or a unit's journal
                            (veho-etcd, veho-patroni, garage)
  help                      this text

settings: lab.env, overridden by lab.local.env and the environment
USAGE
}

# require_lab fails unless "up" has built the lab.
require_lab() {
	if [[ ! -f "$LAB_STATE_DIR/env.json" ]] || ! vm_exists lxd; then
		die "the lab is not built: run lab.sh up"
	fi
}

cmd_up() {
	preflight
	pki_up
	[[ -f "$LAB_STATE_DIR/env.json" ]] || echo '{}' >"$LAB_STATE_DIR/env.json"
	lxd_up
	lxd_vm_up
	section "WAN"
	wan_restore
	db_up
	s3_up
	record
	if ! has_snapshot || [[ "$(env_get '.snapshot_instances // ""')" != "$(inner_names)" ]]; then
		lab_snapshot
	fi
	section "Done"
	info "the lab is up: lab.sh selftest, eval \"\$(test/lab/lab.sh env)\", or lab.sh test"
}

# record writes what was built into env.json.
record() {
	local pg patroni
	pg="$(db_superuser -tAc 'show server_version')"
	patroni="$(inner_exec db-b patroni --version | awk '{print $2}')"
	env_set .recorded_at "$(jq -n --arg v "$(date -u +%Y-%m-%dT%H:%M:%SZ)" '$v')"
	env_set .versions "$(jq -n --arg image "$LAB_IMAGE" --arg pg "$pg" --arg patroni "$patroni" \
		--arg etcd "$LAB_ETCD_VERSION" --arg garage "$LAB_GARAGE_VERSION" \
		--arg inner_lxd "$(inner version 2>/dev/null | awk '/Server version/ {print $3}')" \
		'{image: $image, postgresql: $pg, patroni: $patroni, etcd: $etcd, garage: $garage, inner_lxd: $inner_lxd}')"
	env_set .ca_sha256 "$(jq -n --arg v "$(sha256sum "$LAB_STATE_DIR/pki/ca.crt" | cut -d' ' -f1)" '$v')"
}

cmd_down() {
	confirm "Destroy the veho lab (VM, snapshots, network, project, pool, state)?" || die "cancelled"
	if vm_exists lxd; then
		lxd delete lxd --force && info "VM lxd deleted"
	fi
	lxd_down
	rm -rf "$LAB_STATE_DIR"
	info "state directory removed"
}

cmd_status() {
	section "VM ($LAB_PROJECT)"
	lxd list -c nsN4 -f compact 2>/dev/null || info "no project $LAB_PROJECT"
	if vm_running lxd; then
		section "Containers"
		inner list -c ntsN4 -f compact
		section "Patroni"
		db_patronictl list 2>/dev/null || info "not answering"
		cmd_wan show
	fi
	section "Recorded (env.json)"
	if [[ -f "$LAB_STATE_DIR/env.json" ]]; then jq . "$LAB_STATE_DIR/env.json"; else info "nothing recorded"; fi
}

cmd_env() {
	require_lab
	db_env
	s3_env
	nodes_env
}

cmd_test() {
	require_lab
	local args=() pkgs=()
	while (($#)); do
		case "$1" in
		-run) args+=(-run "$2") && shift 2 ;;
		*) pkgs+=("$1") && shift ;;
		esac
	done
	((${#pkgs[@]})) || pkgs=(./...)
	nodes_stop
	eval "$(cmd_env)"
	cd "$REPO_DIR"
	# -p 1: the packages share one database
	go test -count=1 -p 1 -tags integration "${args[@]}" "${pkgs[@]}"
}

# on NAME ARGS: runs ARGS in the lab VM ("lxd") or in a container.
on() {
	local name="$1"
	shift
	if [[ "$name" == lxd ]]; then lxd exec lxd -- "$@"; else inner_exec "$name" "$@"; fi
}

cmd_ssh() {
	local name="${1:?ssh: lxd or a container?}"
	shift
	if (($#)); then
		on "$name" "$@"
	elif [[ "$name" == lxd ]]; then
		lxd exec lxd -- bash -l
	else
		lxd exec lxd -t -- lxc exec "$name" -- bash -l
	fi
}

cmd_logs() {
	local name="${1:?logs: lxd or a container?}"
	if (($# > 1)); then on "$name" journalctl --no-pager -u "$2"; else on "$name" cat /var/log/cloud-init-output.log; fi
}

main() {
	local command="${1:-help}"
	(($#)) && shift
	load_settings
	ensure_lxd_access "$command" "$@"
	case "$command" in
	preflight) preflight ;;
	up) cmd_up ;;
	down) cmd_down ;;
	status) cmd_status ;;
	env) cmd_env ;;
	test) cmd_test "$@" ;;
	selftest) require_lab && selftest ;;
	wan) cmd_wan "$@" ;;
	psql) require_lab && db_psql "$@" ;;
	schema-reset) require_lab && db_psql -c "drop schema veho cascade; create schema veho authorization veho_migrator" ;;
	nodes) cmd_nodes "$@" ;;
	acceptance) cmd_acceptance "$@" ;;
	campaign) cmd_campaign "$@" ;;
	node) cmd_node "$@" ;;
	db) cmd_db "$@" ;;
	db-certs) require_lab && db_certs_renew ;;
	fault) cmd_fault "$@" ;;
	patronictl) require_lab && db_patronictl "$@" ;;
	etcdctl) require_lab && db_etcdctl "$@" ;;
	garage)
		require_lab
		region_check "${1:-}"
		local x="$1"
		shift
		garage "s3-$x" "$@"
		;;
	snapshot) require_lab && lab_snapshot ;;
	reset) require_lab && lab_reset ;;
	stop) require_lab && lab_stop ;;
	start) require_lab && lab_start ;;
	ssh) cmd_ssh "$@" ;;
	logs) cmd_logs "$@" ;;
	help | -h | --help) usage ;;
	*) usage >&2 && exit 2 ;;
	esac
}

main "$@"
