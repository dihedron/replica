# shellcheck shell=bash disable=SC2015
# The lab's self-test (.specs/veho-lab.md, LAB-10): LAB-4 to LAB-8 on a
# running lab. SC2015 throughout: ok and fail never fail, so A && ok || fail
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
	selftest_db
	selftest_s3
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
