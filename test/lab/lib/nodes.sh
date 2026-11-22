# shellcheck shell=bash
# The veho nodes of lab L3 (.specs/veho-lab-l3.md §2): veho-X1 and veho-X2 in
# each region, under the shipped systemd unit, and the fault-injecting proxy
# in front of each region's Garage. The nodes are not in the baseline
# snapshot: "nodes up" builds them, "nodes down" removes them.

NODE_API_PORT=8443
FAULT_PORT=3910
FAULT_CONTROL=127.0.0.1:3911

# node_names: veho-a1 veho-a2 veho-b1 ... veho-c2.
node_names() {
	local x
	for x in "${REGIONS[@]}"; do printf 'veho-%s1 veho-%s2 ' "$x" "$x"; done
}

# node_check NAME: dies unless NAME is one of the nodes.
node_check() { [[ " $(node_names) " == *" ${1:-} "* ]] || die "no node \"${1:-}\": veho-a1 to veho-c2"; }

# node_port NAME: the port of NAME's API on the lab VM's address: base + 2 x
# (region - 1) + index, so veho-a1 is 8441 and veho-c2 8446.
node_port() { echo $((LAB_API_PORT_BASE + 2 * ($(region_num "${1:5:1}") - 1) + ${1:6:1})); }

# node_dsn ROLE: a node's DSN of veho_ROLE, over the three members' region
# addresses: the node's database traffic crosses the WAN.
node_dsn() {
	local x hosts=""
	for x in "${REGIONS[@]}"; do hosts+="${hosts:+,}$(inner_ip "db-$x"):5432"; done
	db_dsn "$1" "$hosts" /etc/veho/tls/ca.crt
}

# unwrap_key: the lab's unwrap key, 32 random bytes, created once.
unwrap_key() {
	local file="$LAB_STATE_DIR/unwrap.key"
	[[ -f "$file" ]] || (umask 077 && openssl rand 32 >"$file")
	echo "$file"
}

cmd_nodes() {
	require_lab
	case "${1:-}" in
	up) nodes_up ;;
	down) nodes_down ;;
	deploy) nodes_deploy ;;
	*) die "usage: lab.sh nodes up|down|deploy" ;;
	esac
}

# nodes_up creates the six containers and everything a node needs except the
# binary, which "nodes deploy" pushes.
nodes_up() {
	section "veho nodes"
	local name
	for name in $(node_names); do
		pki_issue "$name" "DNS:$name.veho.lab,IP:$(inner_ip "$name"),IP:$(lxd_ip)"
		inner_create "$name" "$LAB_VEHO_MEMORY" -c user.veho-lab=yes
		inner_start "$name"
	done
	for name in $(node_names); do
		inner_wait "$name"
		node_configure "$name"
		inner_proxy "$name" api "$(node_port "$name")" "$NODE_API_PORT"
	done
	env_set .nodes "$(jq -n --arg s "$(node_names)" '$s')"
	info "nodes up: lab.sh nodes deploy builds veho and starts them"
}

# node_configure NAME: the user, the configuration, the TLS files, the
# unwrap key as a systemd credential; "nodes deploy" pushes the unit.
node_configure() {
	local name="$1" x="${1:5:1}" role upper
	inner_push "$REPO_DIR/packaging/postinstall.sh" "$name" /root/postinstall.sh 0700
	inner_exec "$name" /root/postinstall.sh
	inner_exec "$name" install -d -o root -g veho -m 0750 /etc/veho/tls
	inner_push "$LAB_STATE_DIR/pki/ca.crt" "$name" /etc/veho/tls/ca.crt 0644
	inner_push "$LAB_STATE_DIR/pki/$name.crt" "$name" /etc/veho/tls/server.crt 0644
	inner_push "$LAB_STATE_DIR/pki/$name.key" "$name" /etc/veho/tls/server.key 0640
	inner_push "$(unwrap_key)" "$name" /etc/veho/unwrap.key 0600
	inner_exec "$name" chgrp veho /etc/veho/tls/server.key
	inner_write "$name" /etc/veho/veho.yaml 0640 <<YAML
# veho node $name of the lab (test/lab/lib/nodes.sh)
node: {id: $name, region: region-$x}
secrets: {unwrap_key: "systemd-credential:unwrap-key"}
api:
  listen: "$(inner_ip "$name"):$NODE_API_PORT"
  tls: {cert_file: /etc/veho/tls/server.crt, key_file: /etc/veho/tls/server.key}
YAML
	{
		for role in api worker dbos; do
			upper="$(tr '[:lower:]' '[:upper:]' <<<"$role")"
			printf 'VEHO_%s_DATABASE_URL=%s\n' "$upper" "$(node_dsn "$role")"
		done
	} | inner_write "$name" /etc/veho/veho.env 0640
	inner_exec "$name" chgrp veho /etc/veho/veho.yaml /etc/veho/veho.env
	inner_exec "$name" mkdir -p /etc/systemd/system/veho.service.d
	inner_write "$name" /etc/systemd/system/veho.service.d/lab.conf 0644 <<'UNIT'
# the lab's unwrap key (test/lab/lib/nodes.sh)
[Service]
LoadCredential=unwrap-key:/etc/veho/unwrap.key
UNIT
	inner_exec "$name" systemctl daemon-reload
}

# nodes_down removes the nodes.
nodes_down() {
	section "veho nodes"
	local name
	for name in $(node_names); do
		if inner_exists "$name"; then
			[[ "$(inner config get "$name" user.veho-lab)" == yes ]] || die "$name is not the lab's: not removed"
			inner delete "$name" --force >/dev/null && info "$name removed"
		fi
	done
	env_set .nodes '""'
}

# nodes_deploy builds veho and the fault proxy, prepares the database
# (schema, topology, KEK) and restarts the nodes one at a time.
nodes_deploy() {
	section "Deploy"
	[[ "$(env_get '.nodes // ""')" == "$(node_names)" ]] || die "no nodes: run lab.sh nodes up"
	local bin="$LAB_STATE_DIR/bin" name x
	mkdir -p "$bin"
	make -C "$REPO_DIR" compile >"$LAB_STATE_DIR/build.log" 2>&1 || die "make compile failed: see $LAB_STATE_DIR/build.log"
	cp "$REPO_DIR/dist/veho_linux_amd64_v1/veho" "$bin/veho"
	(cd "$REPO_DIR" && CGO_ENABLED=0 go build -trimpath -o "$bin/faultproxy" ./test/lab/faultproxy) || die "cannot build the fault proxy"
	info "built $("$bin/veho" version | tr -s '\n' ' ')"
	for x in "${REGIONS[@]}"; do fault_install "s3-$x"; done
	nodes_database
	for name in $(node_names); do
		inner_exec "$name" systemctl stop veho 2>/dev/null || true
		inner_push "$bin/veho" "$name" /usr/bin/veho 0755
		inner_push "$REPO_DIR/packaging/veho.service" "$name" /usr/lib/systemd/system/veho.service 0644
		node_unblock "$name"
		inner_exec "$name" systemctl daemon-reload
		inner_exec "$name" systemctl enable --now veho >/dev/null 2>&1 || true
		node_wait "$name"
	done
	info "nodes deployed: $(node_names)"
}

# nodes_database migrates the schema, creates the regions of the topology and
# initializes the KEK with the lab's unwrap key, again when the integration
# tests (which use their own key) have replaced it.
nodes_database() {
	local veho="$LAB_STATE_DIR/bin/veho" x current
	export VEHO_MIGRATOR_DATABASE_URL
	VEHO_MIGRATOR_DATABASE_URL="$(db_dsn migrator)"
	"$veho" migrate >/dev/null || die "veho migrate failed"
	for x in "${REGIONS[@]}"; do
		db_psql -qtAc "select veho.put_location('region-$x', 'region', null)" >/dev/null
	done
	current="$(db_psql -tAc "select coalesce(md5(string_agg(wrapped::text, ',' order by version)), '') from veho.kek")"
	if [[ -z "$current" || "$current" != "$(env_get '.kek // ""')" ]]; then
		db_psql -qc "update veho.endpoints set credential_ref = null where credential_ref like 'db://%'" \
			-c "delete from veho.secrets" -c "delete from veho.kek" >/dev/null
		"$veho" kek init --unwrap-key "file:$(unwrap_key)" --audit-sink stderr 2>/dev/null || die "veho kek init failed"
		env_set .kek "$(jq -n --arg v "$(db_psql -tAc "select md5(string_agg(wrapped::text, ',' order by version)) from veho.kek")" '$v')"
		info "KEK initialized with the lab's unwrap key (secrets cleared)"
	fi
}

# node_wait NAME: waits until NAME's API answers /readyz through its
# published port, verifying the lab CA.
node_wait() {
	for _ in $(seq 1 60); do
		curl -fsS --cacert "$LAB_STATE_DIR/pki/ca.crt" "https://$(lxd_ip):$(node_port "$1")/readyz" >/dev/null 2>&1 && return
		sleep 2
	done
	die "$1 is not ready (lab.sh logs $1 veho)"
}

# node_unblock NAME: removes the drop-in of "node kill --stay-down".
node_unblock() {
	inner_exec "$1" sh -c 'if [ -e /run/systemd/system/veho.service.d/stay-down.conf ]; then rm -f /run/systemd/system/veho.service.d/stay-down.conf && systemctl daemon-reload; fi'
}

cmd_node() {
	require_lab
	local verb="${1:-}" name="${2:-}"
	node_check "$name"
	case "$verb" in
	start)
		node_unblock "$name"
		inner_exec "$name" systemctl start veho
		node_wait "$name"
		;;
	stop) inner_exec "$name" systemctl stop veho ;;
	restart)
		node_unblock "$name"
		inner_exec "$name" systemctl restart veho
		node_wait "$name"
		;;
	kill)
		if [[ "${3:-}" == --stay-down ]]; then
			inner_exec "$name" sh -c 'mkdir -p /run/systemd/system/veho.service.d && printf "[Service]\nRestart=no\n" >/run/systemd/system/veho.service.d/stay-down.conf && systemctl daemon-reload'
		fi
		inner_exec "$name" systemctl kill --kill-whom=main --signal=SIGKILL veho
		;;
	pause) inner_exec "$name" systemctl kill --kill-whom=main --signal=SIGSTOP veho ;;
	resume) inner_exec "$name" systemctl kill --kill-whom=main --signal=SIGCONT veho ;;
	*) die "usage: lab.sh node start|stop|restart|kill [--stay-down]|pause|resume NAME" ;;
	esac
}

cmd_db() {
	require_lab
	local verb="${1:-}" x="${2:-}"
	region_check "$x"
	case "$verb" in
	kill) inner stop "db-$x" --force ;;
	start)
		inner_start "db-$x"
		inner_wait "db-$x"
		;;
	*) die "usage: lab.sh db kill|start X" ;;
	esac
}

# fault_install NAME: the fault proxy in an s3 container, under systemd.
fault_install() {
	local name="$1"
	inner_exec "$name" systemctl stop faultproxy 2>/dev/null || true
	inner_push "$LAB_STATE_DIR/bin/faultproxy" "$name" /usr/local/bin/faultproxy 0755
	inner_write "$name" /etc/systemd/system/faultproxy.service 0644 <<UNIT
[Unit]
Description=Fault-injecting S3 proxy (veho lab, test/lab/faultproxy)
After=garage.service

[Service]
ExecStart=/usr/local/bin/faultproxy -listen :$FAULT_PORT -upstream http://127.0.0.1:3900 -control $FAULT_CONTROL
DynamicUser=yes
Restart=on-failure
RestartSec=2

[Install]
WantedBy=multi-user.target
UNIT
	inner_exec "$name" systemctl daemon-reload
	inner_exec "$name" systemctl enable --now faultproxy >/dev/null 2>&1
	inner_proxy "$name" fault "$(published_port "$LAB_FAULT_PORT_BASE" "${name#s3-}")" "$FAULT_PORT"
}

# cmd_fault X MODE [ARGS]: sets, shows or clears the fault of region X's
# proxy (.specs/veho-lab-l3.md §2.2).
cmd_fault() {
	require_lab
	local x="${1:-}" mode="${2:-}" json
	region_check "$x"
	shift 2 || true
	local code=0 for="" count=0 delay="" bucket=""
	case "$mode" in
	status) code="${1:?status: CODE}" && shift ;;
	delay) delay="${1:?delay: DURATION}" && shift ;;
	corrupt) count=1 ;;
	clear)
		inner_exec "s3-$x" curl -fsS -X DELETE "http://$FAULT_CONTROL/fault"
		return
		;;
	show)
		inner_exec "s3-$x" curl -fsS "http://$FAULT_CONTROL/fault"
		return
		;;
	*) die "usage: lab.sh fault X status CODE [--for D] | corrupt [--count N] | delay D | clear | show [--bucket B]" ;;
	esac
	while (($#)); do
		case "$1" in
		--for) for="$2" && shift 2 ;;
		--count) count="$2" && shift 2 ;;
		--bucket) bucket="$2" && shift 2 ;;
		*) die "fault: unknown option $1" ;;
		esac
	done
	json="$(jq -cn --arg mode "$mode" --argjson code "$code" --arg for "$for" --argjson count "$count" \
		--arg delay "$delay" --arg bucket "$bucket" \
		'{mode: $mode, code: $code, for: $for, count: $count, delay: $delay, bucket: $bucket}
		| with_entries(select(.value != "" and .value != 0))')"
	inner_exec "s3-$x" curl -fsS -X POST -H 'Content-Type: application/json' --data "$json" "http://$FAULT_CONTROL/fault"
}
