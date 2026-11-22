# shellcheck shell=bash
# The state store (.specs/veho-lab.md §4): on db-a, db-b and db-c, one etcd
# member, Patroni and PostgreSQL each, PostgreSQL and Patroni from PGDG, etcd
# upstream. db-b bootstraps the cluster; failover prefers b and c.

# db_cloud_init: user data of a db container: no default cluster when
# PostgreSQL is installed (Patroni creates its own).
db_cloud_init() {
	cat <<'YAML'
#cloud-config
write_files:
  - path: /etc/postgresql-common/createcluster.d/veho-lab.conf
    content: "create_main_cluster = false\n"
YAML
}

# db_names: db-a db-b db-c.
db_names() { printf 'db-%s ' "${REGIONS[@]}"; }

# db_etcd_cluster: etcd's initial cluster.
db_etcd_cluster() {
	local x out=""
	for x in "${REGIONS[@]}"; do out+="${out:+,}db-$x=http://$(inner_ip "db-$x"):2380"; done
	echo "$out"
}

# db_etcd_hosts: Patroni's etcd3 hosts.
db_etcd_hosts() {
	local x out=""
	for x in "${REGIONS[@]}"; do out+="${out:+,}$(inner_ip "db-$x"):2379"; done
	echo "$out"
}

db_up() {
	section "State store (etcd, Patroni, PostgreSQL $LAB_PG_MAJOR)"
	local x name
	for x in "${REGIONS[@]}"; do
		name="db-$x"
		pki_issue "$name" "DNS:$name.veho.lab,IP:$(inner_ip "$name"),IP:$(lxd_ip)"
		inner_create "$name" "$LAB_DB_MEMORY" -c cloud-init.user-data="$(db_cloud_init)"
		inner_start "$name"
	done
	for x in "${REGIONS[@]}"; do
		inner_wait "db-$x"
		db_install "db-$x"
		db_configure "db-$x"
	done
	db_etcd_start
	db_patroni_start
	db_database
	for x in "${REGIONS[@]}"; do
		inner_proxy "db-$x" postgres "$(published_port "$LAB_PG_PORT_BASE" "$x")" 5432
		inner_proxy "db-$x" patroni "$(published_port "$LAB_PATRONI_PORT_BASE" "$x")" 8008
	done
	info "PostgreSQL published on $(lxd_ip):$(published_port "$LAB_PG_PORT_BASE" a)-$(published_port "$LAB_PG_PORT_BASE" c), Patroni on $(lxd_ip):$(published_port "$LAB_PATRONI_PORT_BASE" a)-$(published_port "$LAB_PATRONI_PORT_BASE" c)"
}

# db_install NAME: the packages and etcd, once.
db_install() {
	local name="$1" patroni
	if inner_exec "$name" test -x /usr/local/bin/etcd -a -x /usr/bin/patroni; then
		info "$name: packages installed"
		return
	fi
	inner_push "$(pgdg_key)" "$name" /etc/apt/keyrings/pgdg.asc 0644
	inner_write "$name" /etc/apt/sources.list.d/pgdg.sources 0644 <<'SOURCES'
Types: deb
URIs: https://apt.postgresql.org/pub/repos/apt
Suites: noble-pgdg
Components: main
Signed-By: /etc/apt/keyrings/pgdg.asc
SOURCES
	inner_exec "$name" apt-get -qq update >/dev/null || die "$name: apt-get update failed"
	patroni="$(inner_exec "$name" apt-cache madison patroni | awk -v v="$LAB_PATRONI_VERSION" '!found && $3 ~ "^" v "-" {print $3; found = 1}')"
	[[ -n "$patroni" ]] || die "$name: Patroni $LAB_PATRONI_VERSION is not in PGDG any more: update LAB_PATRONI_VERSION"
	inner_exec "$name" env DEBIAN_FRONTEND=noninteractive apt-get -qq install -y --no-install-recommends \
		"postgresql-$LAB_PG_MAJOR" "patroni=$patroni" python3-psycopg2 >/dev/null ||
		die "$name: cannot install PostgreSQL $LAB_PG_MAJOR and Patroni $patroni"
	inner_exec "$name" systemctl disable --now patroni.service >/dev/null 2>&1 || true
	inner_push "$(etcd_release)" "$name" /root/etcd.tar.gz 0600
	inner_exec "$name" sh -c 'tar -xzf /root/etcd.tar.gz -C /usr/local/bin --strip-components=1 --wildcards "*/etcd" "*/etcdctl" "*/etcdutl" && rm /root/etcd.tar.gz' ||
		die "$name: cannot install etcd"
	info "$name: PostgreSQL $LAB_PG_MAJOR, Patroni $patroni, etcd $LAB_ETCD_VERSION installed"
}

# db_configure NAME: certificates, the etcd and Patroni units and Patroni's
# configuration; rewritten on every up, applied by the next start.
db_configure() {
	local name="$1" x ip
	x="${name#db-}" ip="$(inner_ip "$name")"
	inner_exec "$name" sh -c 'mkdir -p /etc/veho-lab /etc/patroni /var/lib/etcd /var/lib/postgresql/'"$LAB_PG_MAJOR"' && chown postgres:postgres /var/lib/postgresql/'"$LAB_PG_MAJOR"
	inner_push "$LAB_STATE_DIR/pki/ca.crt" "$name" /etc/veho-lab/ca.crt 0644
	inner_push "$LAB_STATE_DIR/pki/$name.crt" "$name" /etc/veho-lab/server.crt 0644
	inner_push "$LAB_STATE_DIR/pki/$name.key" "$name" /etc/veho-lab/server.key 0600
	inner_exec "$name" chown postgres:postgres /etc/veho-lab/server.crt /etc/veho-lab/server.key
	inner_write "$name" /etc/systemd/system/veho-etcd.service 0644 <<UNIT
[Unit]
Description=etcd member $name (veho lab)
After=network-online.target
Wants=network-online.target

[Service]
Type=notify
ExecStart=/usr/local/bin/etcd --name $name --data-dir /var/lib/etcd \\
  --listen-peer-urls http://$ip:2380 --initial-advertise-peer-urls http://$ip:2380 \\
  --listen-client-urls http://$ip:2379,http://127.0.0.1:2379 --advertise-client-urls http://$ip:2379 \\
  --initial-cluster $(db_etcd_cluster) --initial-cluster-state new --initial-cluster-token veho-lab \\
  --heartbeat-interval $LAB_ETCD_HEARTBEAT --election-timeout $LAB_ETCD_ELECTION
Restart=on-failure
RestartSec=2

[Install]
WantedBy=multi-user.target
UNIT
	inner_write "$name" /etc/systemd/system/veho-patroni.service 0644 <<'UNIT'
[Unit]
Description=Patroni (veho lab)
After=network-online.target veho-etcd.service
Wants=network-online.target

[Service]
User=postgres
Group=postgres
ExecStart=/usr/bin/patroni /etc/patroni/veho.yml
ExecReload=/bin/kill -HUP $MAINPID
KillMode=process
TimeoutSec=30
Restart=on-failure

[Install]
WantedBy=multi-user.target
UNIT
	local priority=2
	[[ "$x" == a ]] && priority=1
	inner_write "$name" /etc/patroni/veho.yml 0600 <<YAML
# Patroni of the veho lab (test/lab/lib/db.sh, .specs/veho-lab.md §4)
scope: veho
name: $name
restapi:
  listen: 0.0.0.0:8008
  connect_address: $ip:8008
etcd3:
  hosts: $(db_etcd_hosts)
bootstrap:
  dcs:
    ttl: $LAB_PATRONI_TTL
    loop_wait: $LAB_PATRONI_LOOP_WAIT
    retry_timeout: $LAB_PATRONI_RETRY_TIMEOUT
    synchronous_mode: quorum
    synchronous_node_count: 1
    postgresql:
      use_pg_rewind: true
      use_slots: true
      parameters:
        wal_level: replica
        hot_standby: "on"
        wal_log_hints: "on"
        max_connections: 200
        max_wal_senders: 10
        max_replication_slots: 10
        wal_keep_size: 256MB
  initdb:
    - encoding: UTF8
    - data-checksums
postgresql:
  listen: 0.0.0.0:5432
  connect_address: $ip:5432
  use_unix_socket: true
  data_dir: /var/lib/postgresql/$LAB_PG_MAJOR/veho
  bin_dir: /usr/lib/postgresql/$LAB_PG_MAJOR/bin
  authentication:
    superuser:
      username: postgres
      password: $(password postgres)
      sslmode: verify-full
      sslrootcert: /etc/veho-lab/ca.crt
    replication:
      username: replicator
      password: $(password replicator)
      sslmode: verify-full
      sslrootcert: /etc/veho-lab/ca.crt
    rewind:
      username: postgres
      password: $(password postgres)
      sslmode: verify-full
      sslrootcert: /etc/veho-lab/ca.crt
  parameters:
    unix_socket_directories: /var/run/postgresql
    ssl: "on"
    ssl_cert_file: /etc/veho-lab/server.crt
    ssl_key_file: /etc/veho-lab/server.key
    ssl_ca_file: /etc/veho-lab/ca.crt
    ssl_min_protocol_version: TLSv1.3
    password_encryption: scram-sha-256
    tcp_keepalives_idle: 60
    tcp_keepalives_interval: 10
    tcp_keepalives_count: 6
  pg_hba:
    - local all postgres peer
    - hostssl replication replicator $LAB_REGION_PREFIX.0.0/16 scram-sha-256
    - hostssl all postgres $LAB_REGION_PREFIX.0.0/16 scram-sha-256
    - hostssl all postgres 0.0.0.0/0 reject
    - hostssl all all $LAB_REGION_PREFIX.0.0/16 scram-sha-256
    - hostssl all all $LAB_SUBNET scram-sha-256
tags:
  failover_priority: $priority
YAML
	inner_exec "$name" chown postgres:postgres /etc/patroni/veho.yml
	inner_exec "$name" systemctl daemon-reload
	inner_exec "$name" systemctl enable veho-etcd.service veho-patroni.service >/dev/null 2>&1
}

# db_etcd_start starts the three members together (each waits for the
# quorum) and waits until the cluster is healthy.
db_etcd_start() {
	local x
	for x in "${REGIONS[@]}"; do inner_exec "db-$x" systemctl start --no-block veho-etcd.service; done
	for _ in $(seq 1 60); do
		db_etcdctl endpoint health --cluster >/dev/null 2>&1 && break
		sleep 2
	done
	db_etcdctl endpoint health --cluster >/dev/null 2>&1 || die "etcd: the cluster is not healthy (lab.sh logs db-b veho-etcd)"
	info "etcd: three members healthy"
}

# db_etcdctl ARGS: etcdctl on db-b, against the three members.
db_etcdctl() { inner_exec db-b /usr/local/bin/etcdctl --endpoints "$(db_etcd_hosts)" "$@"; }

# db_patronictl ARGS: patronictl in db-b, or in another member when db-b is
# stopped (AC-5 kills the leader's container).
db_patronictl() {
	local name=db-b x
	if ! inner_running db-b; then
		for x in "${REGIONS[@]}"; do inner_running "db-$x" && name="db-$x" && break; done
	fi
	inner_exec "$name" runuser -u postgres -- patronictl -c /etc/patroni/veho.yml "$@"
}

# db_members: Patroni's members as JSON.
db_members() { db_patronictl list -f json </dev/null 2>/dev/null; }

# db_leader: the leader's name, empty when there is none.
db_leader() { db_members | jq -r '.[] | select(.Role == "Leader") | .Member' 2>/dev/null; }

# db_patroni_start starts db-b first, so that it bootstraps the cluster, then
# the others, and waits until both standbys stream.
db_patroni_start() {
	inner_exec db-b systemctl start veho-patroni.service
	db_wait_leader
	local x
	for x in "${REGIONS[@]}"; do inner_exec "db-$x" systemctl start veho-patroni.service; done
	db_wait_ready
}

# db_wait_leader waits until the cluster has a leader.
db_wait_leader() {
	for _ in $(seq 1 90); do
		[[ -n "$(db_leader)" ]] && return
		sleep 2
	done
	die "patroni: no leader after 3 minutes (lab.sh logs db-b veho-patroni)"
}

# db_wait_ready waits until there is a leader and two streaming quorum standbys.
db_wait_ready() {
	local members
	for _ in $(seq 1 120); do
		members="$(db_members)"
		if [[ "$(jq '[.[] | select(.Role == "Leader" and .State == "running")] | length' <<<"$members" 2>/dev/null)" == 1 &&
			"$(jq '[.[] | select(.Role == "Quorum Standby" and .State == "streaming")] | length' <<<"$members" 2>/dev/null)" == 2 ]]; then
			info "patroni: leader $(jq -r '.[] | select(.Role == "Leader") | .Member' <<<"$members"), standbys $(jq -r '[.[] | select(.Role != "Leader") | "\(.Member) (\(.Role))"] | join(", ")' <<<"$members")"
			return
		fi
		sleep 2
	done
	db_patronictl list || true
	die "patroni: the cluster is not ready after 4 minutes"
}

# db_superuser ARGS: psql as postgres over the Unix socket of the leader.
db_superuser() {
	local leader
	leader="$(db_leader)"
	[[ -n "$leader" ]] || die "patroni: no leader"
	inner_exec "$leader" runuser -u postgres -- psql -X "$@"
}

# db_database creates the roles, the database veho and its schemas with
# packaging/postgres/roles.sql, once.
db_database() {
	if [[ "$(db_superuser -tAc "select 1 from pg_roles where rolname = 'veho_worker'" </dev/null)" == 1 ]]; then
		info "database veho and its roles exist"
		return
	fi
	db_superuser -q -v migrator_password="$(password veho_migrator)" -v dbos_password="$(password veho_dbos)" \
		-v api_password="$(password veho_api)" -v worker_password="$(password veho_worker)" -f - \
		<"$REPO_DIR/packaging/postgres/roles.sql" || die "cannot create the database veho and its roles"
	info "database veho and roles veho_migrator, veho_dbos, veho_api, veho_worker created (packaging/postgres/roles.sql)"
}

# db_dsn [ROLE [HOSTS ROOTCERT]]: the DSN of veho_ROLE (default migrator) to
# the primary, verifying the server with the lab CA; by default from the lab
# host, over the published ports of the three regions.
db_dsn() {
	local role="${1:-migrator}" hosts="${2:-}" x
	if [[ -z "$hosts" ]]; then
		for x in "${REGIONS[@]}"; do hosts+="${hosts:+,}$(lxd_ip):$(published_port "$LAB_PG_PORT_BASE" "$x")"; done
	fi
	printf 'postgres://veho_%s:%s@%s/veho?sslmode=verify-full&sslrootcert=%s&target_session_attrs=read-write' \
		"$role" "$(password "veho_$role")" "$hosts" "${3:-$LAB_STATE_DIR/pki/ca.crt}"
}

# db_env: the database environment of the tests on the host.
db_env() {
	local role upper
	printf 'export VEHO_TEST_DATABASE_URL=%q\n' "$(db_dsn migrator)"
	for role in dbos api worker; do
		upper="$(tr '[:lower:]' '[:upper:]' <<<"$role")"
		printf 'export VEHO_TEST_%s_DATABASE_URL=%q\n' "$upper" "$(db_dsn "$role")"
	done
}

# db_psql [--region X] ARGS: psql as veho_migrator over TLS, run in db-b (or
# db-X, connecting to that member only).
db_psql() {
	local hosts="" in=db-b x dsn
	if [[ "${1:-}" == --region ]]; then
		region_check "${2:-}"
		in="db-$2" hosts="$(inner_ip "db-$2"):5432"
		shift 2
	else
		for x in "${REGIONS[@]}"; do hosts+="${hosts:+,}$(inner_ip "db-$x"):5432"; done
	fi
	dsn="$(db_dsn migrator "$hosts" /etc/veho-lab/ca.crt)"
	# one member: that member, primary or standby
	[[ "$hosts" == *,* ]] || dsn="${dsn%&target_session_attrs=read-write}"
	inner_exec "$in" psql -X "$dsn" "$@"
}
