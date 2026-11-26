# shellcheck shell=bash
# The AC-24 campaign (.specs/veho-latency-campaign.md): each cell sets the
# timeouts of its round trip's profile, the WAN's links, waits 5 quiet
# minutes, then runs TestEnvelopeCell (45 minutes observed, 10 injected),
# which appends its row to test/lab/.evidence/envelope/results.csv. The
# plan and the report come from test/envelope.

ENVELOPE_DIR="${LAB_ENVELOPE_DIR:-$LAB_DIR/.evidence/envelope}" # the smoke run (EN-T4) writes elsewhere

envelope() { (cd "$REPO_DIR" && go run ./test/envelope/cmd/envelope "$@"); }

# db_profile [RTT]: the etcd and Patroni timeouts of the round trip's
# profile (veho-latency-envelope.md §4), or the lab's defaults without one;
# Patroni's dynamically, etcd's by a rolling restart, each member back and
# healthy before the next; then both read back (decision 7).
db_profile() {
	local rtt="${1:-}" name etcd_heartbeat etcd_election ttl loop_wait retry_timeout x got
	if [[ -n "$rtt" ]]; then
		eval "$(envelope profile --rtt "$rtt")" || die "db profile: no profile for $rtt ms"
	else
		name=default etcd_heartbeat="$LAB_ETCD_HEARTBEAT" etcd_election="$LAB_ETCD_ELECTION"
		ttl="$LAB_PATRONI_TTL" loop_wait="$LAB_PATRONI_LOOP_WAIT" retry_timeout="$LAB_PATRONI_RETRY_TIMEOUT"
	fi
	db_patronictl edit-config --force --quiet --set "ttl=$ttl" --set "loop_wait=$loop_wait" --set "retry_timeout=$retry_timeout" >/dev/null ||
		die "db profile: patronictl edit-config failed"
	for x in "${REGIONS[@]}"; do
		if ! inner_exec "db-$x" grep -q -- "--heartbeat-interval $etcd_heartbeat --election-timeout $etcd_election\$" /etc/systemd/system/veho-etcd.service; then
			inner_exec "db-$x" sed -i -E "s/--heartbeat-interval [0-9]+ --election-timeout [0-9]+/--heartbeat-interval $etcd_heartbeat --election-timeout $etcd_election/" \
				/etc/systemd/system/veho-etcd.service
			inner_exec "db-$x" systemctl daemon-reload
			inner_exec "db-$x" systemctl restart veho-etcd.service
			db_etcd_wait_healthy || die "db profile: etcd unhealthy after restarting db-$x"
		fi
	done
	got="$(db_patronictl show-config 2>/dev/null | awk '/^(ttl|loop_wait|retry_timeout):/ {printf "%s%s ", $1, $2}')"
	[[ "$got" == *"ttl:$ttl "* && "$got" == *"loop_wait:$loop_wait "* && "$got" == *"retry_timeout:$retry_timeout "* ]] ||
		die "db profile: Patroni reads back \"$got\""
	for x in "${REGIONS[@]}"; do
		inner_exec "db-$x" sh -c "tr '\\0' ' ' </proc/\$(pgrep -x etcd)/cmdline" |
			grep -q -- "--heartbeat-interval $etcd_heartbeat --election-timeout $etcd_election" ||
			die "db profile: db-$x's etcd does not run with the profile's timeouts"
	done
	info "db profile $name: etcd ${etcd_heartbeat}/${etcd_election} ms, Patroni ttl $ttl, loop_wait $loop_wait, retry_timeout $retry_timeout"
}

# db_etcd_wait_healthy: until the three members answer healthy (2 min).
db_etcd_wait_healthy() {
	local i
	for i in $(seq 1 60); do
		[[ "$(db_etcdctl endpoint health -w json 2>/dev/null | jq '[.[] | select(.health)] | length' 2>/dev/null)" == 3 ]] && return 0
		sleep 2
	done
	return 1
}

# envelope_settle: waits for 5 minutes with one etcd leader and term and
# no change of Patroni leader, sampling every 10 s; 20 minutes at most.
envelope_settle() {
	local quiet=0 last="" now i
	for i in $(seq 1 120); do
		now="$(db_etcdctl endpoint status -w json 2>/dev/null | jq -c '[.[] | {leader: .Status.leader, term: .Status.raftTerm}] | unique' 2>/dev/null) $(db_leader 2>/dev/null)"
		if [[ "$now" == "$last" && "$now" != *null* ]]; then
			quiet=$((quiet + 1))
			((quiet >= ${LAB_ENVELOPE_SETTLE_SAMPLES:-30})) && return 0
		else
			quiet=0 last="$now"
		fi
		sleep 10
	done
	return 1
}

# campaign_envelope DIR --phase A|B [--rtt R,R] [--only CELL] [--dry-run] | --report
campaign_envelope() {
	local dir="$1" phase="" rtts="" only="" dry=false observe="${LAB_ENVELOPE_OBSERVE:-45m}" inject="${LAB_ENVELOPE_INJECT:-10m}"
	shift
	local results="$ENVELOPE_DIR/results.csv"
	while (($#)); do
		case "$1" in
		--phase) phase="${2:?--phase: A or B}" && shift 2 ;;
		--rtt) rtts="${2:?--rtt: ms,ms}" && shift 2 ;;
		--only) only="${2:?--only: a cell}" && shift 2 ;;
		--dry-run) dry=true && shift ;;
		--report) envelope report --results "$results" && return ;;
		*) die "usage: lab.sh campaign envelope --phase A|B [--rtt R,R] [--dry-run] | --report" ;;
		esac
	done
	[[ -n "$phase" ]] || die "campaign envelope: --phase A or B"
	local plan
	plan="$(envelope plan --phase "$phase" --rtt "$rtts" --results "$results")" || die "campaign envelope: no plan"
	[[ -z "$only" ]] || plan="$(awk -v c="$only" '$1 == c' <<<"$plan")"
	[[ -n "$plan" ]] || die "campaign envelope: no cell to run"
	if $dry; then
		echo "$plan"
		return
	fi
	mkdir -p "$dir" "$ENVELOPE_DIR"
	section "Campaign envelope: phase $phase, $(wc -l <<<"$plan") cells"
	local id rtt link shifted profile
	while read -r id rtt link shifted profile; do
		[[ -n "$id" ]] || continue
		section "Cell $id ($profile, links $link$([[ "$shifted" == - ]] || echo ", then $shifted"))"
		envelope_db_ready || die "campaign envelope: the database has not two streaming standbys"
		db_profile "$rtt"
		wan_set --link "a-b=$link" --link "a-c=$link" --link "b-c=$link" >/dev/null
		if ! envelope_settle; then
			warn "cell $id: no 5 quiet minutes in 20: recorded invalid"
			ENVELOPE_INVALID=1 envelope_cell "$dir" "$id" "$rtt" "$shifted" "$profile" "$results" 0s 0s
			continue
		fi
		envelope_cell "$dir" "$id" "$rtt" "$shifted" "$profile" "$results" "$observe" "$inject"
	done <<<"$plan"
	wan_set "$LAB_WAN_PROFILE" >/dev/null
	envelope_db_ready || warn "campaign envelope: the database is not back to two streaming standbys"
	db_profile
	envelope report --results "$results" --phase "$phase" | tee "$dir/report.md"
}

# envelope_db_ready: a leader and two streaming standbys, as a killed
# member rejoins (a base backup over the cell's WAN can take long); 30
# minutes at most.
envelope_db_ready() {
	local i
	for i in $(seq 1 180); do
		[[ "$(db_members 2>/dev/null | jq '[.[] | select(.State == "streaming")] | length' 2>/dev/null)" == 2 ]] && return 0
		sleep 10
	done
	return 1
}

# envelope_cell DIR ID RTT SHIFTED PROFILE RESULTS OBSERVE INJECT: one cell's
# measurements, by TestEnvelopeCell.
envelope_cell() {
	local dir="$1"
	LAB_DB_REJOIN_WAIT=900 VEHO_ENVELOPE_CELL="$2" VEHO_ENVELOPE_RTT="$3" VEHO_ENVELOPE_SHIFT="$4" VEHO_ENVELOPE_PROFILE="$5" \
		VEHO_ENVELOPE_RESULTS="$6" VEHO_ENVELOPE_OBSERVE="$7" VEHO_ENVELOPE_INJECT="$8" \
		VEHO_ENVELOPE_INVALID="${ENVELOPE_INVALID:-}" \
		campaign_go "$dir/$2" 2h -run 'TestEnvelopeCell$' >/dev/null 2>&1 ||
		warn "cell $2: TestEnvelopeCell failed (its log: $dir/$2/test.log)"
}
