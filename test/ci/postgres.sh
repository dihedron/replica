#!/usr/bin/env bash
# The integration tests' database without the lab: PostgreSQL in a container,
# with TLS (veho connects only with sslmode=verify-full), the roles, database
# and schemas of packaging/postgres/roles.sql, and the tests' DSNs.
#
#   test/ci/postgres.sh up DIR     start it; DIR/env.sh has the export lines
#   test/ci/postgres.sh down DIR   remove it
#
# CI runs the integration tests with it (.github/workflows/ci.yml); the lab
# (test/lab) covers what it does not: Patroni and the failover, the WAN, the
# S3 backends and the nodes. Those tests skip here.
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cmd="${1:-}" dir="${2:-}"
[[ -n "$dir" ]] || {
	echo "usage: postgres.sh up|down DIR" >&2
	exit 2
}
name="${VEHO_CI_PG_NAME:-veho-ci-postgres}"
port="${VEHO_CI_PG_PORT:-55432}"
image="${VEHO_CI_PG_IMAGE:-postgres:16}" # the lab's major version
container="${CONTAINER:-docker}"

case "$cmd" in
down)
	"$container" rm -f "$name" >/dev/null 2>&1 || true
	rm -rf "$dir"
	exit 0
	;;
up) ;;
*)
	echo "usage: postgres.sh up|down DIR" >&2
	exit 2
	;;
esac

mkdir -p "$dir"
dir="$(cd "$dir" && pwd)"
umask 077

# a CA of its own, and the server's certificate for localhost
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 2 -subj "/CN=veho CI CA" \
	-keyout "$dir/ca.key" -out "$dir/ca.crt" 2>/dev/null
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj "/CN=localhost" \
	-keyout "$dir/server.key" -out "$dir/server.csr" 2>/dev/null
openssl x509 -req -in "$dir/server.csr" -CA "$dir/ca.crt" -CAkey "$dir/ca.key" -days 2 \
	-extfile <(printf 'subjectAltName=DNS:localhost,IP:127.0.0.1\n') -out "$dir/server.crt" 2>/dev/null
chmod 0644 "$dir/ca.crt" "$dir/server.crt"
# the postgres user of the image (uid 999) must own its key
chmod 0600 "$dir/server.key"

su_password="$(openssl rand -hex 16)"
"$container" rm -f "$name" >/dev/null 2>&1 || true
"$container" run -d --name "$name" -p "127.0.0.1:$port:5432" \
	-e POSTGRES_PASSWORD="$su_password" \
	-v "$dir/server.crt:/tls/server.crt:ro" -v "$dir/server.key:/tls/server.key.in:ro" \
	--entrypoint sh "$image" -c 'install -o postgres -m 0600 /tls/server.key.in /tmp/server.key &&
		exec docker-entrypoint.sh postgres -c ssl=on -c ssl_cert_file=/tls/server.crt -c ssl_key_file=/tmp/server.key \
			-c max_connections=300' >/dev/null

for _ in $(seq 60); do
	"$container" exec "$name" pg_isready -q -U postgres -h localhost && break
	sleep 1
done
"$container" exec "$name" pg_isready -q -U postgres -h localhost || {
	"$container" logs "$name" >&2
	exit 1
}

pw() { openssl rand -hex 16; }
migrator="$(pw)" dbos="$(pw)" api="$(pw)" worker="$(pw)"
"$container" exec -i "$name" psql -X -q -U postgres -h localhost -d postgres -v ON_ERROR_STOP=1 \
	-v migrator_password="$migrator" -v dbos_password="$dbos" -v api_password="$api" -v worker_password="$worker" \
	-f - <"$repo/packaging/postgres/roles.sql" >&2

dsn() { printf 'postgres://veho_%s:%s@localhost:%s/veho?sslmode=verify-full&sslrootcert=%s' "$1" "$2" "$port" "$dir/ca.crt"; }
{
	printf 'export VEHO_TEST_DATABASE_URL=%q\n' "$(dsn migrator "$migrator")"
	printf 'export VEHO_TEST_DBOS_DATABASE_URL=%q\n' "$(dsn dbos "$dbos")"
	printf 'export VEHO_TEST_API_DATABASE_URL=%q\n' "$(dsn api "$api")"
	printf 'export VEHO_TEST_WORKER_DATABASE_URL=%q\n' "$(dsn worker "$worker")"
	# one PostgreSQL, no synchronous standby: the tests relax STO-C3 (quorum)
	echo 'export VEHO_TEST_STANDALONE=1'
} >"$dir/env.sh"
echo "PostgreSQL ($image) on localhost:$port with TLS; the DSNs in $dir/env.sh" >&2
