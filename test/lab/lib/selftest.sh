# shellcheck shell=bash disable=SC2015
# The lab's self-test (.specs/veho-lab.md, LAB-10): LAB-4 to LAB-8 on a
# running lab, and L3-T1 to L3-T5 (.specs/veho-lab-l3.md §7) with the nodes. SC2015 throughout: ok and fail never fail, so A && ok || fail
# is an if-then-else. It changes the WAN and switches the primary over, and leaves
# the WAN as it found it.

# ping_rtt FROM TO: the average round trip in ms from container FROM to
# address TO, empty when nothing answers.
ping_rtt() {
	inner_exec "$1" ping -n -q -c 10 -i 0.2 -W 1 "$2" 2>/dev/null | awk -F/ '/^rtt/ {print $5}'
}

# within A B TOL: true when |A - B| <= TOL.
within() { awk -v a="$1" -v b="$2" -v t="$3" 'BEGIN {d = a - b; if (d < 0) d = -d; exit !(d <= t)}'; }

selftest() {
	section "Self-test"
	local saved_links saved_parts
	saved_links="$(env_get '.wan.links // {}')"
	saved_parts="$(env_get '.wan.partitions // []')"
	selftest_wan
	selftest_partition
	wan_apply "$saved_links" >/dev/null
	wan_partition_apply "$saved_parts" >/dev/null
	local pids=""
	[[ -n "$(env_get '.nodes // ""')" ]] && pids="$(node_pids)"
	selftest_db
	selftest_s3
	[[ -n "$pids" ]] && selftest_nodes "$pids"
	((CHECK_FAILS == 0)) || die "self-test: $CHECK_FAILS check(s) failed"
	info "self-test passed"
}

# selftest_wan: LAB-4, with profile fiber1.
selftest_wan() {
	wan_apply "$(wan_profile_links fiber1)" >/dev/null
	wan_partition_apply '[]' >/dev/null
	local delays i pair x y want got
	# shellcheck disable=SC2154 # set by lab.env
	read -r -a delays <<<"$LAB_WAN_PROFILE_fiber1"
	for i in 0 1 2; do
		pair="${WAN_PAIRS[$i]}" x="${pair%-*}" y="${pair#*-}"
		want="$(awk -v d="${delays[$i]}" 'BEGIN {print 2 * d}')"
		got="$(ping_rtt "db-$x" "$(inner_ip "db-$y")")"
		if [[ -n "$got" ]] && within "$got" "$want" 1; then
			ok "LAB-4 $pair: RTT $got ms (profile $want ms)"
		else
			fail "LAB-4 $pair: RTT ${got:-none} ms, profile $want ms"
		fi
	done
	for x in "${REGIONS[@]}"; do
		inner_exists "s3-$x" || continue
		got="$(ping_rtt "db-$x" "$(inner_ip "s3-$x")")"
		if [[ -n "$got" ]] && within "$got" 0 1; then
			ok "LAB-4 inside $x: RTT $got ms"
		else
			fail "LAB-4 inside $x: RTT ${got:-none} ms, expected below 1 ms"
		fi
	done
}

# selftest_partition: LAB-5.
selftest_partition() {
	wan_partition_apply '["a-b","a-c"]' >/dev/null
	[[ -z "$(ping_rtt db-a "$(inner_ip db-b)")" && -z "$(ping_rtt db-c "$(inner_ip db-a)")" ]] &&
		ok "LAB-5 partition a: a cannot reach b or c" || fail "LAB-5 partition a: a still reaches b or c"
	[[ -n "$(ping_rtt db-b "$(inner_ip db-c)")" ]] &&
		ok "LAB-5 partition a: b reaches c" || fail "LAB-5 partition a: b cannot reach c"
	if inner_exists s3-a; then
		[[ -n "$(ping_rtt db-a "$(inner_ip s3-a)")" ]] &&
			ok "LAB-5 partition a: inside a unaffected" || fail "LAB-5 partition a: traffic inside a is cut"
	fi
	wan_partition_apply '[]' >/dev/null
	[[ -n "$(ping_rtt db-a "$(inner_ip db-b)")" ]] &&
		ok "LAB-5 heal: a reaches b again" || fail "LAB-5 heal: a still cannot reach b"
}

# selftest_db: LAB-6 and LAB-7.
selftest_db() {
	local leader names repl published hosts x
	leader="$(db_leader)"
	[[ "$leader" == db-b || "$leader" == db-c ]] &&
		ok "LAB-6 leader $leader" || fail "LAB-6 leader is \"$leader\", not db-b or db-c"
	names="$(db_superuser -tAc 'show synchronous_standby_names')"
	[[ "$names" == "ANY 1 ("* ]] &&
		ok "LAB-6 synchronous_standby_names = $names" || fail "LAB-6 synchronous_standby_names = \"$names\""
	repl="$(db_superuser -tAc "select count(*) from pg_stat_replication where sync_state = 'quorum'")"
	[[ "$repl" == 2 ]] &&
		ok "LAB-6 two quorum standbys stream" || fail "LAB-6 $repl quorum standbys in pg_stat_replication"
	# LAB-7, through the published ports, as the tests on the host connect
	for x in "${REGIONS[@]}"; do hosts+="${hosts:+,}$(lxd_ip):$(published_port "$LAB_PG_PORT_BASE" "$x")"; done
	published() { inner_exec db-a psql -X "$(db_dsn migrator "$hosts" /etc/veho-lab/ca.crt)" -tAc "select inet_server_addr()::text || ' ' || pg_is_in_recovery()" 2>/dev/null; }
	published="$(published)"
	[[ "$published" == "$(inner_ip "$leader")/32 false" ]] &&
		ok "LAB-7 published DSN reaches the primary $leader over TLS" || fail "LAB-7 published DSN answered \"$published\""
	local other=db-c
	[[ "$leader" == db-c ]] && other=db-b
	db_patronictl switchover --leader "$leader" --candidate "$other" --force >/dev/null 2>&1 ||
		fail "LAB-7 switchover to $other refused"
	db_wait_ready >/dev/null
	published="$(published)"
	[[ "$published" == "$(inner_ip "$other")/32 false" ]] &&
		ok "LAB-7 after switchover the DSN reaches the new primary $other" || fail "LAB-7 after switchover the DSN answered \"$published\""
}

# selftest_s3: LAB-8, from the lab host, signed with SigV4 by curl.
selftest_s3() {
	local x bucket url body
	body="veho lab $(date -u +%s)"
	for x in "${REGIONS[@]}"; do
		inner_exists "s3-$x" || continue
		for bucket in "${S3_BUCKETS[@]}"; do
			url="http://$(lxd_ip):$(published_port "$LAB_S3_PORT_BASE" "$x")/$bucket/selftest"
			s3_curl "$x" -X PUT --data-binary "$body" "$url" >/dev/null &&
				[[ "$(s3_curl "$x" "$url")" == "$body" ]] && s3_curl "$x" -X DELETE "$url" >/dev/null &&
				ok "LAB-8 s3-$x/$bucket: put, get, delete" || fail "LAB-8 s3-$x/$bucket: put, get or delete failed"
		done
	done
}

# s3_curl X ARGS: curl signed with region X's key.
s3_curl() {
	local x="$1"
	shift
	curl -fsS --aws-sigv4 "aws:amz:region-$x:s3" \
		--user "$(cat "$LAB_STATE_DIR/passwords/s3-$x-access"):$(cat "$LAB_STATE_DIR/passwords/s3-$x-secret")" \
		-H "x-amz-content-sha256: UNSIGNED-PAYLOAD" "$@"
}

# node_pids: "NAME=PID ..." of the nodes' veho services.
node_pids() {
	local name
	for name in $(node_names); do
		printf '%s=%s ' "$name" "$(inner_exec "$name" systemctl show -p MainPID --value veho)"
	done
}

# node_ready NAME: true when NAME answers /readyz through its published port.
node_ready() { curl -fsS --max-time 5 --cacert "$LAB_STATE_DIR/pki/ca.crt" "https://$(lxd_ip):$(node_port "$1")/readyz" >/dev/null 2>&1; }

# node_active NAME: true when NAME's veho service is active.
node_active() { [[ "$(inner_exec "$1" systemctl is-active veho 2>/dev/null)" == active ]]; }

# selftest_nodes PIDS: L3-T1 to L3-T5; PIDS from node_pids before the
# switchover of selftest_db. Kills and restarts veho-c2.
selftest_nodes() {
	local entry name pid victim=veho-c2 x url body md5 code
	for entry in $1; do
		name="${entry%=*}" pid="${entry#*=}"
		node_ready "$name" && ok "L3-T1 $name answers on $(lxd_ip):$(node_port "$name")" || fail "L3-T1 $name does not answer"
		[[ "$(inner_exec "$name" systemctl show -p MainPID --value veho)" == "$pid" ]] && node_ready "$name" &&
			ok "L3-T2 $name followed the switchover without a restart" || fail "L3-T2 $name restarted or stopped across the switchover"
	done

	# L3-T3
	cmd_node kill "$victim" --stay-down
	sleep 12
	! node_active "$victim" && ok "L3-T3 $victim stays down after kill --stay-down" || fail "L3-T3 $victim restarted after kill --stay-down"
	cmd_node start "$victim" >/dev/null && ok "L3-T3 $victim back after node start" || fail "L3-T3 $victim did not start"
	pid="$(inner_exec "$victim" systemctl show -p MainPID --value veho)"
	cmd_node kill "$victim"
	for _ in $(seq 1 30); do
		node_ready "$victim" && [[ "$(inner_exec "$victim" systemctl show -p MainPID --value veho)" != "$pid" ]] && break
		sleep 1
	done
	node_ready "$victim" && ok "L3-T3 systemd restarted $victim after kill" || fail "L3-T3 $victim was not restarted after kill"

	# L3-T4, through the published proxies
	body="veho lab fault $(date -u +%s)"
	md5="$(printf '%s' "$body" | openssl md5 -binary | base64)"
	for x in "${REGIONS[@]}"; do
		url="http://$(lxd_ip):$(published_port "$LAB_FAULT_PORT_BASE" "$x")/dest-1/selftest"
		s3_curl "$x" -X PUT -H "Content-MD5: $md5" --data-binary "$body" "$url" >/dev/null &&
			[[ "$(s3_curl "$x" "$url")" == "$body" ]] &&
			ok "L3-T4 proxy $x forwards a signed put and get" || fail "L3-T4 proxy $x does not forward"
		cmd_fault "$x" status 503 --for 3s
		code="$(s3_curl "$x" -o /dev/null -w '%{http_code}' "$url" 2>/dev/null || true)"
		[[ "$code" == 503 ]] && ok "L3-T4 proxy $x answers 503 during the fault" || fail "L3-T4 proxy $x answered $code during the fault"
		sleep 4
		s3_curl "$x" "$url" >/dev/null && ok "L3-T4 proxy $x forwards after the fault" || fail "L3-T4 proxy $x still faulty after --for"
		cmd_fault "$x" corrupt
		code="$(s3_curl "$x" -X PUT -H "Content-MD5: $md5" --data-binary "$body" -o /dev/null -w '%{http_code}' "$url" 2>/dev/null || true)"
		[[ "$code" == 400 ]] && ok "L3-T4 proxy $x: Garage refuses the corrupted part" || fail "L3-T4 proxy $x: corrupted put answered $code"
		cmd_fault "$x" status 503
		cmd_fault "$x" clear
		s3_curl "$x" -X DELETE "$url" >/dev/null && ok "L3-T4 proxy $x forwards after clear" || fail "L3-T4 proxy $x still faulty after clear"
	done

	# L3-T5
	inner_exec "$victim" sh -c 'mv /etc/systemd/system/veho.service.d/lab.conf /root/lab.conf && systemctl daemon-reload && systemctl restart veho' || true
	sleep 3
	! node_ready "$victim" && inner_exec "$victim" journalctl -u veho --since -10s --no-pager | grep -q "unwrap" &&
		ok "L3-T5 $victim without the unwrap key refuses to start" || fail "L3-T5 $victim started without the unwrap key, or said nothing"
	inner_exec "$victim" sh -c 'mv /root/lab.conf /etc/systemd/system/veho.service.d/lab.conf && systemctl daemon-reload'
	cmd_node restart "$victim" >/dev/null
	node_ready "$victim" && ok "L3-T5 $victim back with the key" || fail "L3-T5 $victim did not come back"
}
