# shellcheck shell=bash
# The S3 destinations (.specs/veho-lab.md §5): on s3-a, s3-b and s3-c, one
# Garage node each, every one its own cluster, with the buckets source,
# dest-1, dest-2 and dest-3 and the key veho-lab.

S3_BUCKETS=(source dest-1 dest-2 dest-3)

s3_up() {
	section "S3 destinations (Garage $LAB_GARAGE_VERSION)"
	local x
	for x in "${REGIONS[@]}"; do
		inner_create "s3-$x" "$LAB_S3_MEMORY"
		inner_start "s3-$x"
	done
	for x in "${REGIONS[@]}"; do
		inner_wait "s3-$x"
		s3_install "s3-$x"
		s3_layout "s3-$x"
		s3_buckets "s3-$x"
		inner_proxy "s3-$x" s3 "$(published_port "$LAB_S3_PORT_BASE" "$x")" 3900
	done
	info "S3 published on $(lxd_ip):$(published_port "$LAB_S3_PORT_BASE" a)-$(published_port "$LAB_S3_PORT_BASE" c)"
}

# garage NAME ARGS: the garage CLI in an s3 container.
garage() {
	local name="$1"
	shift
	inner_exec "$name" /usr/local/bin/garage -c /etc/garage.toml "$@"
}

# s3_install NAME: the binary, its configuration and its unit; started.
s3_install() {
	local name="$1" x
	x="${name#s3-}"
	if ! inner_exec "$name" test -x /usr/local/bin/garage; then
		inner_push "$(garage_binary)" "$name" /usr/local/bin/garage 0755
	fi
	inner_exec "$name" mkdir -p /var/lib/garage/meta /var/lib/garage/data
	inner_write "$name" /etc/garage.toml 0600 <<TOML
# Garage of the veho lab (test/lab/lib/s3.sh, .specs/veho-lab.md §5)
metadata_dir = "/var/lib/garage/meta"
data_dir = "/var/lib/garage/data"
db_engine = "lmdb"
replication_factor = 1
rpc_bind_addr = "0.0.0.0:3901"
rpc_public_addr = "$(inner_ip "$name"):3901"
rpc_secret = "$(s3_secret "$name-rpc" 32)"

[s3_api]
s3_region = "region-$x"
api_bind_addr = "0.0.0.0:3900"

[admin]
api_bind_addr = "0.0.0.0:3903"
admin_token = "$(s3_secret "$name-admin" 32)"
TOML
	inner_write "$name" /etc/systemd/system/garage.service 0644 <<'UNIT'
[Unit]
Description=Garage (veho lab)
After=network-online.target
Wants=network-online.target

[Service]
Environment=RUST_LOG=garage=info
ExecStart=/usr/local/bin/garage -c /etc/garage.toml server
Restart=on-failure
RestartSec=2

[Install]
WantedBy=multi-user.target
UNIT
	inner_exec "$name" systemctl daemon-reload
	inner_exec "$name" systemctl enable garage.service >/dev/null 2>&1
	inner_exec "$name" systemctl restart garage.service
	s3_wait "$name"
}

# s3_secret NAME BYTES: a hex secret of the state directory, created once.
s3_secret() {
	local file="$LAB_STATE_DIR/passwords/$1"
	if [[ ! -f "$file" ]]; then
		mkdir -p "${file%/*}" && chmod 0700 "${file%/*}"
		(umask 077 && openssl rand -hex "$2" >"$file")
	fi
	cat "$file"
}

# s3_wait NAME: waits until Garage answers its CLI.
s3_wait() {
	for _ in $(seq 1 30); do
		garage "$1" status >/dev/null 2>&1 && return
		sleep 1
	done
	die "$1: Garage does not answer (lab.sh logs $1 garage)"
}

# s3_layout NAME: the one-node layout, once.
s3_layout() {
	local name="$1" id
	if garage "$name" layout show 2>/dev/null | grep -q 'Current cluster layout version: [1-9]'; then
		return
	fi
	id="$(garage "$name" node id -q | cut -d@ -f1)"
	[[ -n "$id" ]] || die "$name: no Garage node id"
	garage "$name" layout assign -z "${name#s3-}1" -c 20G "$id" >/dev/null
	garage "$name" layout apply --version 1 >/dev/null
	info "$name: layout applied (node ${id:0:16})"
}

# s3_buckets NAME: the buckets and the key veho-lab, once; records the key.
s3_buckets() {
	local name="$1" bucket info
	if ! garage "$name" key info veho-lab >/dev/null 2>&1; then
		garage "$name" key create veho-lab >/dev/null
	fi
	for bucket in "${S3_BUCKETS[@]}"; do
		garage "$name" bucket info "$bucket" >/dev/null 2>&1 || garage "$name" bucket create "$bucket" >/dev/null
		garage "$name" bucket allow --read --write --owner "$bucket" --key veho-lab >/dev/null
	done
	info="$(garage "$name" key info veho-lab --show-secret)"
	(
		umask 077
		awk -F': *' '/^Key ID/ {print $2}' <<<"$info" >"$LAB_STATE_DIR/passwords/$name-access"
		awk -F': *' '/^Secret key/ {print $2}' <<<"$info" >"$LAB_STATE_DIR/passwords/$name-secret"
	)
	[[ -s "$LAB_STATE_DIR/passwords/$name-access" && -s "$LAB_STATE_DIR/passwords/$name-secret" ]] ||
		die "$name: cannot read the key veho-lab"
	info "$name: buckets ${S3_BUCKETS[*]}, key veho-lab"
}

# s3_env: the S3 environment of the tests on the host.
s3_env() {
	local x upper
	for x in "${REGIONS[@]}"; do
		upper="$(tr '[:lower:]' '[:upper:]' <<<"$x")"
		printf 'export VEHO_TEST_S3_%s_ENDPOINT=%q\n' "$upper" "http://$(lxd_ip):$(published_port "$LAB_S3_PORT_BASE" "$x")"
		printf 'export VEHO_TEST_S3_%s_REGION=%q\n' "$upper" "region-$x"
		printf 'export VEHO_TEST_S3_%s_ACCESS_KEY=%q\n' "$upper" "$(cat "$LAB_STATE_DIR/passwords/s3-$x-access")"
		printf 'export VEHO_TEST_S3_%s_SECRET_KEY=%q\n' "$upper" "$(cat "$LAB_STATE_DIR/passwords/s3-$x-secret")"
	done
}
