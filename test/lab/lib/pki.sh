# shellcheck shell=bash
# The lab PKI: a CA and the servers' certificates. Everything stays in the
# state directory. Copied from ligo 9f11da9 (test/lab/lib/pki.sh): the
# passwords moved to common.sh (password), no client certificates.

pki_up() {
	local dir="$LAB_STATE_DIR/pki"
	mkdir -p "$dir"
	chmod 0700 "$LAB_STATE_DIR" "$dir"
	if [[ ! -f "$dir/ca.crt" ]]; then
		openssl req -x509 -new -nodes -newkey ec -pkeyopt ec_paramgen_curve:P-256 \
			-keyout "$dir/ca.key" -out "$dir/ca.crt" -days 3650 -subj "/CN=veho lab CA" 2>/dev/null
		chmod 0600 "$dir/ca.key"
		info "lab CA created"
	fi
}

# pki_issue NAME SANS: issues a certificate NAME.crt/.key from the lab CA,
# once, with SANS (an openssl subjectAltName value), for server and client
# authentication: the nodes present theirs to each other when they forward
# API calls (.specs/veho-forwarding.md §2.5). One without client
# authentication, from before, is issued again.
pki_issue() {
	local dir="$LAB_STATE_DIR/pki" name="$1" sans="$2"
	[[ -f "$dir/$name.crt" ]] && openssl x509 -in "$dir/$name.crt" -noout -ext extendedKeyUsage 2>/dev/null | grep -q 'Client Authentication' && return
	openssl req -new -nodes -newkey ec -pkeyopt ec_paramgen_curve:P-256 \
		-keyout "$dir/$name.key" -out "$dir/$name.csr" -subj "/CN=$name.veho.lab" 2>/dev/null
	openssl x509 -req -in "$dir/$name.csr" -CA "$dir/ca.crt" -CAkey "$dir/ca.key" -CAcreateserial \
		-days 365 -out "$dir/$name.crt" \
		-extfile <(printf 'subjectAltName=%s\nextendedKeyUsage=serverAuth,clientAuth\n' "$sans") 2>/dev/null
	rm -f "${dir:?}/${name:?}.csr"
	chmod 0600 "$dir/$name.key"
	info "certificate issued for $name ($sans)"
}
