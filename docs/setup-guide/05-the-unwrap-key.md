# The unwrap key

Tenants store the credentials of their storage systems in veho as **secrets**: an S3 access key, for example, which the transfers use and nobody can read back. Secrets are kept in the database under **envelope encryption**:

- each secret is encrypted with its own data key (AES-256-GCM);
- each data key is wrapped by the **key-encryption key** (KEK), which is stored in the database too, wrapped;
- the KEK is wrapped by the **unwrap key**, which is *not* in the database: each node reads it at start, from a file or a systemd credential.

A copy of the database, a backup or a WAL archive is therefore useless without the unwrap key. Every node needs the same unwrap key; keep a copy offline, as losing it loses every stored secret.

## Making the key

The unwrap key is 32 random bytes. Make it once, on a trusted host:

```bash
umask 077
openssl rand 32 > unwrap.key
```

## Giving it to the nodes

The recommended way is an **encrypted systemd credential**, which systemd decrypts for the service only, into a directory no other process can read. On each node:

```bash
sudo systemd-creds encrypt --name=unwrap-key unwrap.key /etc/veho/unwrap-key.cred
sudo mkdir -p /etc/systemd/system/veho.service.d
sudo tee /etc/systemd/system/veho.service.d/unwrap-key.conf >/dev/null <<'EOF'
[Service]
LoadCredentialEncrypted=unwrap-key:/etc/veho/unwrap-key.cred
EOF
sudo systemctl daemon-reload
```

and in `veho.yaml`:

```yaml
secrets:
  unwrap_key: "systemd-credential:unwrap-key"
```

`systemd-creds encrypt` binds the credential to the host's key (and to its TPM, where there is one), so the file cannot be decrypted elsewhere. Where encrypted credentials are not available (inside some containers), `LoadCredential=unwrap-key:/etc/veho/unwrap.key` with a root-owned file of mode 0600 gives the same isolation from other processes, without the encryption at rest.

The alternative is a plain file: `unwrap_key: "file:/etc/veho/unwrap.key"`. veho refuses a file that is not a regular file, that group or others can read, or that belongs neither to root nor to the service's user.

## Initialising the KEK

Once, after `veho migrate`, from an administrator's host that has the unwrap key:

```bash
export VEHO_MIGRATOR_DATABASE_URL='postgres://veho_migrator:<password>@db-a.example:5432,db-b.example:5432,db-c.example:5432/veho?sslmode=verify-full&sslrootcert=ca.crt&target_session_attrs=read-write'
veho kek init --unwrap-key file:./unwrap.key
```

This creates KEK version 1, wrapped by the unwrap key. A node refuses to start when it cannot unwrap the active KEK with its own unwrap key, so a node with the wrong key, or none, never runs.

Rotating the KEK, re-wrapping the data keys and replacing the unwrap key are routine procedures of the *Operator's Guide*.
