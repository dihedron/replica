# A first replication

This chapter creates a tenant and its administrator, registers three S3 buckets, one per region, and replicates an object from the first to the two others.

## The first tenant and its administrator

Tenants are created by platform administrators through the API, but the very first ones come straight from the database, as `veho_migrator`:

```bash
export VEHO_MIGRATOR_DATABASE_URL='postgres://veho_migrator:<password>@db-a.example:5432,db-b.example:5432,db-c.example:5432/veho?sslmode=verify-full&sslrootcert=ca.crt&target_session_attrs=read-write'
veho bootstrap tenant acme
veho bootstrap apikey --tenant acme --role admin --name alice > alice.key
chmod 600 alice.key
```

`veho bootstrap apikey` creates the service account `alice` and prints its new API key once; veho stores only its hash. The key expires in 90 days (`--expires` shortens it); running the command again with the same `--name` adds a key to the account, which is how a bootstrap key is rotated. `--platform-admin` instead of `--tenant` makes a platform administrator, who manages tenants and nothing inside them. Identities from an OIDC provider or a SPIFFE trust domain get their tenant and role from **entitlements** (`veho bootstrap entitlement`, then `veho entitlement create` through the API).

## The command-line client

The same binary is the client. It takes its connection from the environment or from options, and its credential never from the command line (the process list would show it):

```bash
export VEHO_API_URL=https://veho-a1.example:8443
export VEHO_CA_FILE=/path/to/ca.crt
export VEHO_TOKEN_FILE=$PWD/alice.key       # or VEHO_TOKEN, or --cert-file/--key-file
veho endpoint list
```

`--api rest` uses the REST API instead of gRPC; `-o json` prints the API's JSON.

## Credentials, as a secret

The S3 key of each region, as a JSON object, from a file or standard input, never as an argument:

```bash
printf '{"access_key_id": "%s", "secret_access_key": "%s"}' "$KEY_A" "$SECRET_A" | veho secret set s3-a --stdin
```

Repeat for `s3-b` and `s3-c`. `veho secret show s3-a` shows the secret's version and dates; nothing ever shows its value.

## Endpoints

An endpoint is a storage location the tenant registers once: a driver, its configuration, the location in the topology, and the secret of its credentials. Jobs name endpoints, never URLs.

```bash
cat > a.json <<'EOF'
{"endpoint": "https://s3.region-a.example", "region": "region-a", "bucket": "images", "path_style": true}
EOF
veho endpoint create a-images --driver s3 --location region-a --config-file a.json --credential-ref db://s3-a
veho endpoint test a-images     # writes, reads and deletes a probe object
```

Register `b-images` and `c-images` the same way. For the `s3` driver, `endpoint` may be empty for AWS itself; `path_style` puts the bucket in the path, as most S3-compatible services want; `storage_class` (optional) is the class of the copies written there (see the Operator's Guide, storage classes). Unknown keys are refused. The `file` driver takes `{"root": "/srv/replicas"}`.

## A policy

A policy says where the copies go and when the job is done. Save it as a named, versioned policy, so that jobs refer to it:

```bash
cat > geo.json <<'EOF'
{
  "destinations": [
    {"endpoint": "b-images", "path": "golden/ubuntu-24.04.qcow2"},
    {"endpoint": "c-images", "path": "golden/ubuntu-24.04.qcow2"}
  ],
  "completion": {"min_copies": 2, "spread": [{"level": "region", "min_distinct": 2}]}
}
EOF
veho policy validate --file geo.json
veho policy create geo --file geo.json
```

## The job

```bash
veho job submit --source a-images:golden/ubuntu-24.04.qcow2 --policy geo --label release=24.04
veho job wait 01a2...        # exit 0 when it succeeded, 3 failed, 4 cancelled
veho job show 01a2...
```

`veho job watch` prints each change as it happens. `veho job wait` exits 0 once the job has succeeded: both copies exist, under their final names, and each one's digest matches the source's.

A job can also carry its policy inline (`--policy-file geo.json`), when the tenant allows it, and an idempotency key (`--idempotency-key`), so that a client that repeats a submission after a timeout gets the first job back instead of a second one.
