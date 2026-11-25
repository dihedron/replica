# shellcheck shell=bash
# DevStack on the VM: Keystone, Glance and Swift behind the TLS proxy, the
# test project and its credentials, and a smoke test from the lab host.

readonly DEVSTACK_DIR=/opt/stack/devstack
readonly DEVSTACK_MARKER=/opt/stack/.lab-stacked
readonly DEVSTACK_UNIT=lab-stack
readonly DEVSTACK_LOG=/opt/stack/logs/stack.sh.log
# DevStack's CA chain, which signs its TLS proxy's certificates
readonly DEVSTACK_CA=/opt/stack/data/CA/int-ca/ca-chain.pem
# the project, user and application credential of the tests
readonly TEST_PROJECT=veho-test
readonly TEST_USER=veho-test

devstack_stacked() { vm_ssh "test -f $DEVSTACK_MARKER" 2>/dev/null; }

# devstack_up: installs DevStack and runs stack.sh, unless already done.
devstack_up() {
	if devstack_stacked; then
		info "DevStack is already running"
		return
	fi
	local branch state
	branch="$(env_get .versions.devstack_branch)"
	state="$(vm_ssh "systemctl show -p ActiveState --value $DEVSTACK_UNIT" 2>/dev/null || true)"
	case "$state" in
	active | activating) info "stack.sh is already running" ;;
	failed | *)
		if [[ "$state" == failed ]]; then
			info "a previous stack.sh failed: unstacking, then stacking again"
			vm_ssh "sudo systemctl reset-failed $DEVSTACK_UNIT; cd $DEVSTACK_DIR && sudo -u stack HOME=/opt/stack ./unstack.sh >/dev/null 2>&1 || true"
		fi
		info "installing DevStack $branch"
		vm_ssh "bash -s -- $(printf '%q ' "$branch" "$DEVSTACK_DIR")" <<'SCRIPT'
set -euo pipefail
branch="$1" dir="$2"
sudo apt-get update -q >/dev/null
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -q git >/dev/null
[[ -d /tmp/devstack-bootstrap ]] || git clone -q --depth 1 -b "$branch" https://opendev.org/openstack/devstack /tmp/devstack-bootstrap
id stack >/dev/null 2>&1 || sudo /tmp/devstack-bootstrap/tools/create-stack-user.sh >/dev/null
[[ -d "$dir" ]] || sudo -u stack git clone -q -b "$branch" https://opendev.org/openstack/devstack "$dir"
SCRIPT
		devstack_local_conf | vm_ssh "sudo -u stack tee $DEVSTACK_DIR/local.conf >/dev/null"
		env_set .versions.devstack_commit "$(vm_ssh "sudo -u stack git -C $DEVSTACK_DIR rev-parse HEAD")"
		info "running stack.sh (about 30 minutes; follow it with: lab.sh logs)"
		vm_ssh "sudo systemd-run --quiet --unit=$DEVSTACK_UNIT -p User=stack -p RemainAfterExit=yes \
			-p WorkingDirectory=$DEVSTACK_DIR --setenv=HOME=/opt/stack \
			/bin/bash -c './stack.sh && touch $DEVSTACK_MARKER'"
		;;
	esac
	devstack_wait
}

# devstack_local_conf: Keystone, Glance (the import workflow, two file
# stores) and Swift (one replica), every API behind the TLS proxy. Glance's
# web-download may reach any port: the sources of the tests are on the lab's
# addresses and ports.
devstack_local_conf() {
	local password ip
	password="$(secret admin-password)"
	ip="$(vm_ip)"
	cat <<CONF
[[local|localrc]]
ADMIN_PASSWORD=$password
DATABASE_PASSWORD=$password
RABBIT_PASSWORD=$password
SERVICE_PASSWORD=$password
SWIFT_HASH=$(secret swift-hash)
SWIFT_REPLICAS=1
HOST_IP=$ip
SERVICE_HOST=$ip
LOGFILE=$DEVSTACK_LOG
LOG_COLOR=False
disable_all_services
enable_service mysql rabbit key g-api tls-proxy
enable_service s-proxy s-object s-container s-account
GLANCE_USE_IMPORT_WORKFLOW=True
GLANCE_ENABLE_MULTIPLE_STORES=True
GLANCE_MULTIPLE_FILE_STORES=fast,cheap
GLANCE_DEFAULT_BACKEND=fast

[[post-config|\$GLANCE_API_CONF]]
[import_filtering_opts]
allowed_ports = []
CONF
	# DevStack makes the file stores only without Swift: the lab has both
	glance_stores_conf
}

# glance_stores_conf: the two file stores, which DevStack leaves out when
# Swift is enabled.
glance_stores_conf() {
	cat <<'CONF'
[DEFAULT]
enabled_backends = fast:file,cheap:file
[glance_store]
default_backend = fast
[fast]
filesystem_store_datadir = /opt/stack/data/glance/fast/
[cheap]
filesystem_store_datadir = /opt/stack/data/glance/cheap/
CONF
}

# glance_stores_live: the same, on a running DevStack stacked without it.
glance_stores_live() {
	vm_ssh "sudo grep -q '^enabled_backends' /etc/glance/glance-api.conf" && return
	info "configuring Glance's file stores fast and cheap"
	local script
	script="$(cat <<'SCRIPT'
set -euo pipefail
source "$1/inc/ini-config"
section=""
while IFS= read -r line; do
	if [[ "$line" =~ ^\[(.*)\]$ ]]; then section="${BASH_REMATCH[1]}"; continue; fi
	iniset -sudo /etc/glance/glance-api.conf "$section" "${line%% = *}" "${line#* = }"
done
install -d -o stack -g stack /opt/stack/data/glance/fast /opt/stack/data/glance/cheap
systemctl restart devstack@g-api
SCRIPT
)"
	glance_stores_conf | vm_ssh "sudo bash -c $(printf '%q' "$script") lab $DEVSTACK_DIR"
}


# devstack_wait: waits for stack.sh, reporting its progress.
devstack_wait() {
	local deadline=$((SECONDS + 7200)) state last="" line
	while :; do
		if devstack_stacked; then
			info "stack.sh completed"
			return
		fi
		state="$(vm_ssh "systemctl show -p ActiveState --value $DEVSTACK_UNIT" 2>/dev/null || echo unknown)"
		case "$state" in
		failed | inactive)
			vm_ssh "sudo tail -n 30 $DEVSTACK_LOG" 2>/dev/null | sed 's/^/        | /'
			die "stack.sh failed (full log: lab.sh logs)"
			;;
		esac
		((SECONDS < deadline)) || die "stack.sh did not complete within 2 hours"
		line="$(vm_ssh "sudo grep -E '^[0-9-]+ [0-9:.]+ \| ' $DEVSTACK_LOG.summary 2>/dev/null | tail -n1" 2>/dev/null || true)"
		if [[ -n "$line" && "$line" != "$last" ]]; then
			info "stack.sh: ${line#* | }"
			last="$line"
		fi
		sleep 30
	done
}

# devstack_openstack ARGS...: the openstack CLI on the VM, as admin in the
# admin project, or as OS_LAB_USER (with its password) in OS_LAB_PROJECT.
devstack_openstack() {
	local auth
	if [[ -n "${OS_LAB_USER:-}" ]]; then
		auth="source $DEVSTACK_DIR/openrc admin admin >/dev/null 2>&1; export OS_USERNAME=$OS_LAB_USER OS_PROJECT_NAME=$OS_LAB_PROJECT OS_PASSWORD=$(secret test-password)"
	else
		auth="source $DEVSTACK_DIR/openrc admin admin >/dev/null 2>&1"
	fi
	vm_ssh "cd /opt/stack && sudo -u stack bash -c $(printf '%q' "set +u; $auth; set -u; openstack $(printf '%q ' "$@")")"
}

keystone_url() { echo "https://$(vm_ip)/identity/v3"; }
ca_file() { echo "$LAB_STATE_DIR/ca.pem"; }

# devstack_configure: the tests' project, user (member) and application
# credential, and DevStack's CA on the lab host.
devstack_configure() {
	glance_stores_live
	vm_ssh "sudo cat $DEVSTACK_CA" >"$(ca_file)"
	devstack_openstack project show "$TEST_PROJECT" >/dev/null 2>&1 ||
		devstack_openstack project create --domain Default --description "veho tests" "$TEST_PROJECT" >/dev/null
	devstack_openstack user show --domain Default "$TEST_USER" >/dev/null 2>&1 ||
		devstack_openstack user create --domain Default --project "$TEST_PROJECT" --password "$(secret test-password)" "$TEST_USER" >/dev/null
	devstack_openstack role add --user "$TEST_USER" --project "$TEST_PROJECT" member
	local id
	id="$(OS_LAB_USER=$TEST_USER OS_LAB_PROJECT=$TEST_PROJECT devstack_openstack application credential show -f value -c id veho-test 2>/dev/null || true)"
	if [[ -z "$id" ]]; then
		info "creating the application credential of $TEST_USER"
		id="$(OS_LAB_USER=$TEST_USER OS_LAB_PROJECT=$TEST_PROJECT devstack_openstack application credential create \
			--secret "$(secret app-cred-secret)" -f value -c id veho-test)"
	fi
	env_set .openstack.auth_url "$(keystone_url)"
	env_set .openstack.region RegionOne
	env_set .openstack.project "$TEST_PROJECT"
	env_set .openstack.user "$TEST_USER"
	env_set .openstack.app_cred_id "$id"
}

# devstack_smoke: from the lab host, as the tests reach it: a token with the
# application credential, Glance's import methods and stores, Swift's SLO.
devstack_smoke() {
	local token catalog glance swift
	token="$(curl -fsS --cacert "$(ca_file)" -D - -o "$LAB_STATE_DIR/catalog.json" -H 'Content-Type: application/json' \
		-d "$(jq -n --arg id "$(env_get .openstack.app_cred_id)" --arg s "$(secret app-cred-secret)" \
			'{auth: {identity: {methods: ["application_credential"], application_credential: {id: $id, secret: $s}}}}')" \
		"$(keystone_url)/auth/tokens" | awk 'tolower($1) == "x-subject-token:" { print $2 }' | tr -d '\r')" ||
		die "no token from Keystone at $(keystone_url) with the application credential"
	[[ -n "$token" ]] || die "Keystone answered without a token"
	catalog="$LAB_STATE_DIR/catalog.json"
	glance="$(jq -r '.token.catalog[] | select(.type == "image") | .endpoints[] | select(.interface == "public") | .url' "$catalog")"
	swift="$(jq -r '.token.catalog[] | select(.type == "object-store") | .endpoints[] | select(.interface == "public") | .url' "$catalog")"
	rm -f "$catalog"
	[[ -n "$glance" && -n "$swift" ]] || die "the catalog lacks Glance or Swift"
	ok "Keystone: a token with the application credential"
	local methods stores slo
	methods="$(curl -fsS --cacert "$(ca_file)" -H "X-Auth-Token: $token" "$glance/v2/info/import" | jq -r '.["import-methods"].value | join(",")')" ||
		die "Glance at $glance does not answer"
	[[ "$methods" == *glance-direct* && "$methods" == *web-download* ]] || die "Glance's import methods are $methods"
	stores="$(curl -fsS --cacert "$(ca_file)" -H "X-Auth-Token: $token" "$glance/v2/info/stores" | jq -r '[.stores[].id] | join(",")')"
	[[ "$stores" == *fast* && "$stores" == *cheap* ]] || die "Glance's stores are $stores"
	ok "Glance: $glance, import methods $methods, stores $stores"
	slo="$(curl -fsS --cacert "$(ca_file)" "${swift%%/v1*}/info" | jq -r 'has("slo")')"
	[[ "$slo" == true ]] || die "Swift at $swift has no SLO"
	ok "Swift: $swift, with SLO"
}
