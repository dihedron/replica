# shellcheck shell=bash
# The Nexus node (.specs/veho-nexus.md §6): Sonatype Nexus Repository
# Community Edition on nexus-a, from Sonatype's archive with its own Java,
# behind nginx with a certificate of the lab CA; raw hosted repositories
# veho-raw (redeploy allowed) and veho-raw-once (refused), a user veho with
# the rights on raw repositories only, and anonymous reading. Optional:
# "lab.sh nexus up" adds it, "lab.sh nexus down" removes it, so the rest of
# the lab does not carry its memory.

NEXUS=nexus-a

# nexus_api METHOD PATH [CURL ARGS]: Nexus's REST API as admin, from inside
# the container.
nexus_api() {
	local method="$1" path="$2"
	shift 2
	inner_exec "$NEXUS" curl -fsS -u "admin:$(password nexus-admin)" -X "$method" \
		-H 'Content-Type: application/json' "http://127.0.0.1:8081/service/rest/v1/$path" "$@"
}

nexus_up() {
	section "Nexus ($LAB_NEXUS_VERSION)"
	inner_create "$NEXUS" "$LAB_NEXUS_MEMORY"
	inner_start "$NEXUS"
	inner_wait "$NEXUS"
	nexus_install
	nexus_wait
	nexus_configure
	nexus_tls
	inner_proxy "$NEXUS" nexus "$LAB_NEXUS_PORT" 443
	info "Nexus published on https://$(lxd_ip):$LAB_NEXUS_PORT (user veho)"
}

nexus_down() {
	inner_exists "$NEXUS" || return 0
	inner delete "$NEXUS" --force >/dev/null
	rm -f "$LAB_STATE_DIR/passwords/nexus-admin" "$LAB_STATE_DIR/passwords/nexus-veho"
	info "$NEXUS removed"
}

# nexus_install: the archive in /opt/nexus, its data in /opt/sonatype-work,
# a 1 GiB heap, the unit; started.
nexus_install() {
	if ! inner_exec "$NEXUS" test -x /opt/nexus/bin/nexus; then
		local url="https://download.sonatype.com/nexus/3/nexus-$LAB_NEXUS_VERSION-linux-x86_64.tar.gz"
		inner_push "$(fetch "$url" "$LAB_NEXUS_SHA256")" "$NEXUS" /root/nexus.tar.gz 0600
		inner_exec "$NEXUS" sh -c 'set -e
			id nexus >/dev/null 2>&1 || useradd --system --home-dir /opt/sonatype-work --shell /usr/sbin/nologin nexus
			mkdir -p /opt/nexus /opt/sonatype-work
			tar -xzf /root/nexus.tar.gz -C /opt/nexus --strip-components=1 --wildcards "nexus-*/"
			rm /root/nexus.tar.gz
			chown -R nexus:nexus /opt/sonatype-work'
		inner_write "$NEXUS" /opt/nexus/bin/nexus.vmoptions 0644 <<'OPTIONS'
-Xms1024m
-Xmx1024m
-XX:MaxDirectMemorySize=1024m
-XX:+UnlockDiagnosticVMOptions
-XX:+LogVMOutput
-XX:LogFile=../sonatype-work/nexus3/log/jvm.log
-XX:-OmitStackTraceInLastException
-Djava.io.tmpdir=../sonatype-work/nexus3/tmp
-Dkaraf.data=../sonatype-work/nexus3
-Dkaraf.log=../sonatype-work/nexus3/log
OPTIONS
		inner_write "$NEXUS" /etc/systemd/system/nexus.service 0644 <<'UNIT'
[Unit]
Description=Nexus Repository (veho lab)
After=network.target

[Service]
Type=forking
User=nexus
LimitNOFILE=65536
ExecStart=/opt/nexus/bin/nexus start
ExecStop=/opt/nexus/bin/nexus stop
Restart=on-abort

[Install]
WantedBy=multi-user.target
UNIT
	fi
	inner_exec "$NEXUS" systemctl daemon-reload
	inner_exec "$NEXUS" systemctl enable --now nexus >/dev/null 2>&1
}

# nexus_wait: until the REST API answers (a first start takes minutes).
nexus_wait() {
	local i
	for i in $(seq 1 120); do
		inner_exec "$NEXUS" curl -fsS -o /dev/null http://127.0.0.1:8081/service/rest/v1/status 2>/dev/null && return
		((i % 10 == 0)) && info "waiting for Nexus to start ($((i * 5)) s)"
		sleep 5
	done
	die "Nexus did not start in 10 minutes (lab.sh logs $NEXUS nexus)"
}

# nexus_configure: the admin password, the EULA of the Community Edition,
# anonymous reading, the repositories, the role and the user veho.
nexus_configure() {
	if inner_exec "$NEXUS" test -f /opt/sonatype-work/nexus3/admin.password; then
		local initial
		initial="$(inner_exec "$NEXUS" cat /opt/sonatype-work/nexus3/admin.password)"
		inner_exec "$NEXUS" curl -fsS -u "admin:$initial" -X PUT -H 'Content-Type: text/plain' \
			--data "$(password nexus-admin)" http://127.0.0.1:8081/service/rest/v1/security/users/admin/change-password
		inner_exec "$NEXUS" rm -f /opt/sonatype-work/nexus3/admin.password
	fi
	local eula
	if eula="$(nexus_api GET system/eula 2>/dev/null)" && [[ "$(jq -r .accepted <<<"$eula")" != true ]]; then
		nexus_api POST system/eula --data "$(jq -c '.accepted = true' <<<"$eula")" >/dev/null
	fi
	nexus_api PUT security/anonymous --data '{"enabled": true, "userId": "anonymous", "realmName": "NexusAuthorizingRealm"}' >/dev/null
	local repo policy
	for repo in veho-raw:allow veho-raw-once:allow_once; do
		policy="${repo#*:}" repo="${repo%%:*}"
		nexus_api GET "repositories/$repo" >/dev/null 2>&1 && continue
		nexus_api POST repositories/raw/hosted --data "$(jq -cn --arg n "$repo" --arg p "$policy" \
			'{name: $n, online: true, storage: {blobStoreName: "default", strictContentTypeValidation: false, writePolicy: $p}}')" >/dev/null
		info "repository $repo (write policy $policy)"
	done
	nexus_api GET security/roles/veho-writer >/dev/null 2>&1 ||
		nexus_api POST security/roles --data '{"id": "veho-writer", "name": "veho-writer", "description": "veho lab: raw repositories", "privileges": ["nx-repository-view-raw-*-*"], "roles": []}' >/dev/null
	if [[ "$(nexus_api GET 'security/users?userId=veho' | jq 'length')" == 0 ]]; then
		nexus_api POST security/users --data "$(jq -cn --arg p "$(password nexus-veho)" \
			'{userId: "veho", firstName: "veho", lastName: "lab", emailAddress: "veho@lab.invalid", password: $p, status: "active", roles: ["veho-writer"]}')" >/dev/null
		info "user veho"
	fi
}

# nexus_tls: nginx in front of Nexus on 443, with the lab CA's certificate
# for the lab VM's address (where the tests reach it) and the container's.
nexus_tls() {
	pki_issue "$NEXUS" "DNS:$NEXUS.veho.lab,IP:$(lxd_ip),IP:$(inner_ip "$NEXUS")"
	inner_exec "$NEXUS" sh -c 'command -v nginx >/dev/null || DEBIAN_FRONTEND=noninteractive apt-get install -y -q nginx-light >/dev/null'
	inner_push "$LAB_STATE_DIR/pki/$NEXUS.crt" "$NEXUS" /etc/nginx/nexus.crt 0644
	inner_push "$LAB_STATE_DIR/pki/$NEXUS.key" "$NEXUS" /etc/nginx/nexus.key 0600
	inner_write "$NEXUS" /etc/nginx/sites-enabled/default 0644 <<'NGINX'
server {
	listen 443 ssl;
	ssl_certificate /etc/nginx/nexus.crt;
	ssl_certificate_key /etc/nginx/nexus.key;
	client_max_body_size 0;
	proxy_request_buffering off;
	proxy_buffering off;
	location / {
		proxy_pass http://127.0.0.1:8081;
		proxy_set_header Host $http_host;
		proxy_set_header X-Forwarded-Proto https;
		proxy_read_timeout 600s;
	}
}
NGINX
	inner_exec "$NEXUS" systemctl reload-or-restart nginx
}

# nexus_env: the tests' environment, when the node is up.
nexus_env() {
	inner_running "$NEXUS" || return 0
	printf 'export VEHO_TEST_NEXUS_URL=%q\n' "https://$(lxd_ip):$LAB_NEXUS_PORT"
	printf 'export VEHO_TEST_NEXUS_USER=veho\n'
	printf 'export VEHO_TEST_NEXUS_PASSWORD=%q\n' "$(password nexus-veho)"
	printf 'export VEHO_TEST_NEXUS_CA=%q\n' "$LAB_STATE_DIR/pki/ca.crt"
	printf 'export VEHO_TEST_NEXUS_ALLOW=%q\n' "$(lxd_ip)/32"
}
