# shellcheck shell=bash
# The proxy node (.specs/veho-proxy.md §7): Squid on proxy-a, in region a,
# with a second interface on the bridge "outside", and its own dnsmasq that
# alone resolves s3.outside.veho.lab; a Garage on out-s3, behind nginx with a
# certificate of the lab CA for that name, on "outside" only. The regions
# have no route to "outside" (table inet veho_outside of the lab VM), and
# their DNS does not know the name: the veho nodes reach it only through
# Squid. Optional: "lab.sh proxy up" adds it, "lab.sh proxy down" removes it.

PROXY=proxy-a
OUTSIDE=out-s3
OUTSIDE_NAME=s3.outside.veho.lab
OUTSIDE_GATEWAY=10.70.0.1
OUTSIDE_PROXY_IP=10.70.0.10
OUTSIDE_S3_IP=10.70.0.20
PROXY_PORT=3128

proxy_up() {
	section "Proxy (Squid) and the outside S3"
	proxy_network
	inner_create "$PROXY" "$LAB_PROXY_MEMORY"
	inner_start "$PROXY"
	inner_wait "$PROXY"
	proxy_install
	outside_s3
	info "Squid on $(inner_ip "$PROXY"):$PROXY_PORT; $OUTSIDE_NAME ($OUTSIDE_S3_IP) only through it"
	info "redeploy the nodes to give them the proxy: lab.sh nodes deploy"
}

proxy_down() {
	local n
	for n in "$OUTSIDE" "$PROXY"; do
		inner_exists "$n" && inner delete "$n" --force >/dev/null
	done
	inner network show outside >/dev/null 2>&1 && inner network delete outside >/dev/null
	lxd exec lxd -- nft delete table inet veho_outside 2>/dev/null || true
	rm -f "$LAB_STATE_DIR/passwords/$OUTSIDE-access" "$LAB_STATE_DIR/passwords/$OUTSIDE-secret"
	info "proxy and outside S3 removed"
}

# proxy_network: the bridge "outside", without NAT, and no forwarding
# between it and the regions.
proxy_network() {
	inner network show outside >/dev/null 2>&1 ||
		inner network create outside ipv4.address="$OUTSIDE_GATEWAY/24" ipv4.nat=false ipv6.address=none dns.domain=outside.veho.lab >/dev/null
	lxd exec lxd -- bash -euc "
		nft delete table inet veho_outside 2>/dev/null || true
		nft add table inet veho_outside
		nft add chain inet veho_outside forward '{ type filter hook forward priority -20; policy accept; }'
		nft add rule inet veho_outside forward iifname outside drop
		nft add rule inet veho_outside forward oifname outside drop
	" || die "proxy: cannot isolate the bridge outside"
}

# proxy_install: the second interface, Squid and dnsmasq.
proxy_install() {
	inner config device get "$PROXY" eth1 type >/dev/null 2>&1 ||
		inner config device add "$PROXY" eth1 nic network=outside name=eth1 ipv4.address="$OUTSIDE_PROXY_IP" >/dev/null
	inner_write "$PROXY" /etc/netplan/60-outside.yaml 0600 <<'NETPLAN'
network:
  version: 2
  ethernets:
    eth1:
      dhcp4: true
      dhcp4-overrides: {use-routes: false, use-dns: false}
NETPLAN
	inner_exec "$PROXY" netplan apply
	inner_exec "$PROXY" sh -c 'export DEBIAN_FRONTEND=noninteractive NEEDRESTART_SUSPEND=1
		command -v squid >/dev/null && command -v dnsmasq >/dev/null ||
			{ apt-get update -q && apt-get install -y -q squid dnsmasq; } >/dev/null'
	inner_write "$PROXY" /etc/dnsmasq.d/veho-outside.conf 0644 <<DNSMASQ
# only the outside name: the "external DNS" of the proxy alone
listen-address=127.0.0.1
bind-interfaces
no-resolv
address=/$OUTSIDE_NAME/$OUTSIDE_S3_IP
DNSMASQ
	inner_write "$PROXY" /etc/squid/squid.conf 0644 <<SQUID
# Squid of the veho lab (test/lab/lib/proxy.sh): CONNECT to 443 from the regions
http_port $PROXY_PORT
dns_nameservers 127.0.0.1
acl regions src $LAB_REGION_PREFIX.0.0/16
acl tls_port port 443
acl CONNECT method CONNECT
http_access deny CONNECT !tls_port
http_access allow CONNECT regions
http_access deny all
access_log daemon:/var/log/squid/access.log squid
cache deny all
SQUID
	inner_exec "$PROXY" systemctl restart dnsmasq squid
}

# outside_s3: Garage and nginx on out-s3, installed while it is on region a
# (for the packages), then moved to "outside" only.
outside_s3() {
	if ! inner_exists "$OUTSIDE"; then
		lxd exec lxd -- timeout 300 lxc init lab-container "$OUTSIDE" -p region-a -c limits.memory="$LAB_S3_MEMORY" \
			-c boot.autostart=true -d eth0,ipv4.address="$LAB_REGION_PREFIX.1.60" </dev/null >/dev/null || die "cannot create $OUTSIDE"
	fi
	if [[ "$(inner config device get "$OUTSIDE" eth0 network 2>/dev/null)" != outside ]]; then
		inner_start "$OUTSIDE"
		inner_wait "$OUTSIDE"
		s3_install "$OUTSIDE" 127.0.0.1 outside # one node, no peers: its RPC stays local
		pki_issue "$OUTSIDE" "DNS:$OUTSIDE_NAME"
		inner_push "$LAB_STATE_DIR/pki/$OUTSIDE.crt" "$OUTSIDE" /etc/nginx-s3.crt 0644
		inner_push "$LAB_STATE_DIR/pki/$OUTSIDE.key" "$OUTSIDE" /etc/nginx-s3.key 0600
		inner_exec "$OUTSIDE" sh -c 'export DEBIAN_FRONTEND=noninteractive NEEDRESTART_SUSPEND=1
			command -v nginx >/dev/null || { apt-get update -q && apt-get install -y -q nginx; } >/dev/null
			mv /etc/nginx-s3.crt /etc/nginx-s3.key /etc/nginx/'
		inner_write "$OUTSIDE" /etc/nginx/sites-enabled/default 0644 <<NGINX
server {
	listen 443 ssl;
	server_name $OUTSIDE_NAME;
	ssl_certificate /etc/nginx/nginx-s3.crt;
	ssl_certificate_key /etc/nginx/nginx-s3.key;
	client_max_body_size 0;
	proxy_request_buffering off;
	proxy_buffering off;
	location / {
		proxy_pass http://127.0.0.1:3900;
		proxy_set_header Host \$http_host;
		proxy_read_timeout 600s;
	}
}
NGINX
		inner_exec "$OUTSIDE" systemctl reload-or-restart nginx
		inner stop "$OUTSIDE" >/dev/null
		inner config device set "$OUTSIDE" eth0 network=outside ipv4.address="$OUTSIDE_S3_IP" >/dev/null
	fi
	inner_start "$OUTSIDE"
	inner_wait "$OUTSIDE"
	s3_wait "$OUTSIDE"
	s3_layout "$OUTSIDE"
	s3_buckets "$OUTSIDE"
}

# proxy_settings: the s3 driver's proxy, for the nodes' configuration,
# when the node is up (lib/nodes.sh).
proxy_settings() {
	inner_running "$PROXY" || return 0
	printf ', proxy: {url: "http://%s:%s", hosts: [%s]}' "$(inner_ip "$PROXY")" "$PROXY_PORT" "$OUTSIDE_NAME"
}

# proxy_requests: how many CONNECTs Squid has logged.
proxy_requests() { inner_exec "$PROXY" sh -c 'grep -c CONNECT /var/log/squid/access.log || true'; }

# proxy_env: the tests' environment, when the node is up.
proxy_env() {
	inner_running "$PROXY" || return 0
	printf 'export VEHO_TEST_OUTSIDE_ENDPOINT=%q\n' "https://$OUTSIDE_NAME"
	printf 'export VEHO_TEST_OUTSIDE_IP_ENDPOINT=%q\n' "https://$OUTSIDE_S3_IP"
	printf 'export VEHO_TEST_OUTSIDE_ACCESS_KEY=%q\n' "$(cat "$LAB_STATE_DIR/passwords/$OUTSIDE-access")"
	printf 'export VEHO_TEST_OUTSIDE_SECRET_KEY=%q\n' "$(cat "$LAB_STATE_DIR/passwords/$OUTSIDE-secret")"
}
