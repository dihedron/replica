# shellcheck shell=bash
# The long campaigns of lab L3 (.specs/veho-lab-l3.md §6), run overnight and
# unattended (AGENTS.md rule 14). Each writes its log and summary to
# test/lab/.evidence/campaign-NAME-DATE/.

cmd_campaign() {
	require_lab
	local name="${1:-}" dir
	shift || true
	dir="$LAB_DIR/.evidence/campaign-$name-$(date -u +%Y%m%dT%H%M)"
	case "$name" in
	soak) campaign_soak "$dir" "$@" ;;
	wan24) campaign_wan24 "$dir" "$@" ;;
	long) campaign_long "$dir" ;;
	*) die "usage: lab.sh campaign soak [--duration D] [--jobs N] | wan24 [--hours H] | long" ;;
	esac
}

# campaign_go DIR TIMEOUT ARGS: the campaign's go test, its output in DIR.
campaign_go() {
	local dir="$1" timeout="$2"
	shift 2
	mkdir -p "$dir"
	nodes_start
	eval "$(cmd_env)"
	(cd "$REPO_DIR" && go test -count=1 -p 1 -v -timeout "$timeout" -tags "acceptance campaign" "$@" ./test/acceptance) 2>&1 | tee "$dir/test.log"
	return "${PIPESTATUS[0]}"
}

# campaign_soak DIR: AC-17, 50 jobs under chaos for 8 hours.
campaign_soak() {
	local dir="$1" duration=8h jobs=50
	shift
	while (($#)); do
		case "$1" in
		--duration) duration="$2" && shift 2 ;;
		--jobs) jobs="$2" && shift 2 ;;
		*) die "campaign soak: unknown option $1" ;;
		esac
	done
	section "Campaign soak: $jobs jobs under chaos for $duration"
	wan_set fiber1 >/dev/null
	LAB_SOAK_DURATION="$duration" LAB_SOAK_JOBS="$jobs" campaign_go "$dir" 24h -run TestCampaignSoak
}

# campaign_long DIR: AC-1 at 4 GiB and AC-8 with a 30-minute outage. The
# matrix asks for 10 GiB, whose copies would need 30 GiB in the lab VM and
# its disk grown past what the host has free; 4 GiB (64 parts, 12 GiB of
# copies) was decided on 2026-11-22 (veho-lab-l3.md §6).
campaign_long() {
	local dir="$1" free
	free="$(lxd exec lxd -- df --output=avail -BG / | tail -1 | tr -dc 0-9)"
	((free >= 16)) || die "campaign long: the lab VM has ${free} GiB free, 16 are needed"
	section "Campaign long: AC-1 at 4 GiB, AC-8 with a 30-minute outage"
	wan_set fiber1 >/dev/null
	LAB_AC1_SIZE=4294967296 LAB_AC8_OUTAGE=30m campaign_go "$dir" 4h -run 'TestAC01|TestAC08'
}

# campaign_wan24 DIR: AC-18. Light load (one job at a time, no chaos) while
# the WAN switches between fiber1 and fiber2 every 2 hours for H hours (24),
# then an hour each at rtt20, rtt30 and rtt40. Every minute it records
# etcd's leader and raft term and Patroni's leader; a leader change, a new
# raft term or a new Patroni timeline is a failure.
campaign_wan24() {
	local dir="$1" hours=24
	shift
	[[ "${1:-}" == --hours ]] && hours="$2"
	mkdir -p "$dir"
	section "Campaign wan24: ${hours}h of fiber1/fiber2, then rtt20, rtt30, rtt40"
	local total=$((hours + 3)) start
	start="$(date +%s)"
	LAB_SOAK_DURATION="${total}h" LAB_SOAK_JOBS=1 LAB_SOAK_CHAOS=0 campaign_go "$dir" "$((total + 2))h" -run TestCampaignSoak >/dev/null 2>&1 &
	local load=$!
	db_patronictl history -f json >"$dir/patroni-history-before.json" 2>/dev/null
	local profile=fiber1 next_switch=$((start + 7200)) now
	wan_set "$profile" >/dev/null
	while (($(date +%s) < start + total * 3600)); do
		now="$(date +%s)"
		if ((now >= start + hours * 3600)); then
			case $(((now - start - hours * 3600) / 3600)) in
			0) profile=rtt20 ;; 1) profile=rtt30 ;; *) profile=rtt40 ;;
			esac
			wan_set "$profile" >/dev/null
		elif ((now >= next_switch)); then
			[[ "$profile" == fiber1 ]] && profile=fiber2 || profile=fiber1
			wan_set "$profile" >/dev/null
			next_switch=$((now + 7200))
		fi
		printf '%s %s etcd=%s patroni=%s\n' "$(date -u +%FT%TZ)" "$profile" \
			"$(db_etcdctl endpoint status -w json 2>/dev/null | jq -c '[.[] | {leader: .Status.leader, term: .Status.raftTerm}] | unique' 2>/dev/null)" \
			"$(db_leader 2>/dev/null)" >>"$dir/samples.log"
		sleep 60
	done
	db_patronictl history -f json >"$dir/patroni-history-after.json" 2>/dev/null
	wan_set fiber1 >/dev/null
	wait "$load"
	local leaders terms timelines
	leaders="$(awk -F'"leader":' '{print $2}' "$dir/samples.log" | cut -d, -f1 | sort -u | grep -c .)"
	terms="$(grep -o '"term":[0-9]*' "$dir/samples.log" | sort -u | grep -c .)"
	timelines=$(($(jq length "$dir/patroni-history-after.json") - $(jq length "$dir/patroni-history-before.json")))
	{
		echo "etcd leaders seen: $leaders (want 1)"
		echo "raft terms seen: $terms (want 1)"
		echo "new Patroni timelines: $timelines (want 0)"
	} | tee "$dir/summary.txt"
	((leaders == 1 && terms == 1 && timelines == 0))
}
