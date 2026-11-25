# veho: distribution of the drivers as plugins

This file specifies which storage drivers are built into `veho` and how the others are shipped, trusted, configured and started as process plugins. It revises OI-10 and is part of `veho_specification.md`; the plugin protocol itself is `veho-plugins.md` §6.

Status: approved, 2026-11-24; revised and approved 2026-11-25 (§11: the trusted keys on disk, third-party plugins signed by their publishers).

## 1. Decisions (2026-11-24)

1. **Only drivers that need nothing but the Go standard library are built in**: `file` and `http` (`veho-sources.md`). Every driver with third-party dependencies is a separate binary, `veho-plugin-<name>`, started as a process plugin: `s3` (the AWS SDK, 68 packages, leaves the core), `sftp`, `ftp`, and every later one. This revises OI-10's decision of the same morning (all built-ins compiled in); `go-plugin` and its gRPC protocol are already in the core, so the rule adds no dependency to it.
2. **A plugin binary is trusted through a signed manifest**, installed with it by its package; the operator no longer copies checksums into the configuration (PLG-14's manifest, taken from backlog 8).
3. **Platform settings reach a driver through a `settings` block per driver** in veho's configuration. A plugin validates its own settings when it starts; a refusal stops `veho serve` at start, with the plugin's message.
4. **No key is compiled into `veho`** (revision of 2026-11-25, §11). Every publisher's public keys, veho's own included, are files in one directory, `/etc/veho/plugin-keys/`; a driver names the key that signs it, `veho` by default. A third-party publisher signs its plugins as the release does, with `tools/plugin-manifest`.

The data path cost was measured (PLG-17): 325 MB/s out of process against 380 MB/s in process, about 15 %, well above what one transfer gets on a WAN link.

## 2. Configuration

The `plugins` list becomes `drivers`, one list for the built-in drivers and the plugins. **A driver not listed is not available**: no endpoint of it can be created, and existing ones fail their transfers with a `Permanent` error naming the missing driver.

```yaml
drivers:
  - name: http                      # built in
    settings: {allow_cidrs: [10.20.0.0/16]}
  - name: file                      # built in
    settings: {roots: [/mnt/images]}
  - name: s3                        # plugin from its package: /usr/libexec/veho/plugins/veho-plugin-s3
  - name: sftp
    settings: {allow_cidrs: [10.20.0.0/16]}
  - name: acme                      # a third-party plugin, signed by its publisher
    key: acme                       # /etc/veho/plugin-keys/acme.pub
  - name: beta                      # a third-party plugin pinned by the operator
    path: /opt/beta/veho-plugin-beta
    sha256: 9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08
```

- `name`: a built-in driver, or a plugin. For a plugin without `path`, the binary is `/usr/libexec/veho/plugins/veho-plugin-<name>` and its manifest `veho-plugin-<name>.manifest` next to it (§3).
- `key`: the name of the key file in `/etc/veho/plugin-keys/` (without `.pub`) whose keys must have signed the plugin's manifest; default `veho`, the release's. A key signs only the drivers that name it: a publisher's key cannot sign `s3` unless the configuration says so. Same syntax as `name`; not allowed on a built-in driver or with `path`.
- `path` and `sha256`: a plugin pinned by the operator, without a manifest. Both or neither.
- `settings`: a JSON object, passed to the driver as is: to a built-in driver's constructor, to a plugin at start (§4). Default `{}`.
- Validation: names unique; a name that is neither built in nor installed fails the configuration check, which runs before the node joins.

## 3. The signed manifest

### 3.1 Contents

```json
{"name": "s3", "version": "0.4.0", "veho": "[0.4.0]", "sha256": "…64 hex…", "protocol": 1}
```

- `version`: the plugin's own version, shown in the log at start.
- `veho`: the versions of `veho` the plugin supports, a range in interval notation (`pkg/semver`): `[` and `]` include the bound, `(` and `)` exclude it, an empty bound is unbounded and takes `(` or `)`, a single version in brackets is that version alone. `[0.5.0,0.6.0)` is 0.5.0 up to, not including, 0.6.0; `[0.5.0,)` is 0.5.0 or later; `[0.4.0]` is 0.4.0 only. Versions are Semantic Versioning 2.0 (`MAJOR.MINOR.PATCH`, an optional pre-release, build metadata ignored), compared by its precedence, so `[0.5.0,0.6.0)` excludes `0.5.0-rc.1` and includes `0.5.1-rc.1` and `0.6.0-rc.1` (which precedes `0.6.0`); a publisher that must exclude the pre-releases of 0.6.0 writes `[0.5.0,0.6.0-0)`. The release signs its plugins with `[<its version>]`.

Signed with **Ed25519**: `veho-plugin-<name>.manifest` holds the JSON above, `veho-plugin-<name>.manifest.sig` the signature of its exact bytes, base64. Ed25519 is in the standard library; verifying the release's OpenPGP signature in the core would need a third-party OpenPGP library, against decision 1.

### 3.2 Verification at start

For a plugin without `path`, `veho serve`:

1. reads the manifest and its signature from the root-owned plugin directory (PLG-14: the directory and the files owned by root, not writable by others);
2. reads the driver's key file, `/etc/veho/plugin-keys/<key>.pub` (a list of keys, so that a key can be rotated over one release), from a directory and a file owned by root and not writable by others, and verifies the signature with its keys;
3. checks that `name` is the configured name and that **`veho`'s version is in the manifest's `veho` range**: the release's plugins carry `[<release>]`, so only that release's plugins run with the release key, and an older signed plugin with a known flaw cannot be put back (a downgrade);
4. starts the binary through go-plugin's `SecureConfig` with the manifest's `sha256`, which hashes the file before running it, as today.

A failure at any step stops `veho serve` with the step and the plugin's name. A development build has a version of its own (a snapshot, a pre-release), which no release manifest's range contains, so it runs only plugins pinned by `path` and `sha256`. A missing key file, a key file of no valid key, or one with wrong ownership fails closed.

### 3.3 Signing in the release

- A new key pair: the private key in the repository secret `PLUGIN_SIGNING_KEY` (and in the offline backup of the packaging key, R-09), the public key in `packaging/plugin-signing.pub`, which the `veho` package installs as `/etc/veho/plugin-keys/veho.pub` (§5).
- `tools/plugin-manifest` (a small Go program, standard library only) writes and signs the manifests after goreleaser builds the binaries, in the release workflow; the workflow checks the signature with the published public key, as it checks the packaging key today.
- The manifests and their signatures are covered by `checksums.txt`, so the existing OpenPGP signature of the release covers them too.

## 4. Settings and the plugin's start

- Settings reach a plugin in the environment variable `VEHO_PLUGIN_SETTINGS` (the JSON object), set by the core when it starts the process; nothing else of veho's configuration reaches the plugin (PLG-15).
- `process.Serve` becomes `process.Serve(func(settings json.RawMessage) (plugin.Driver, error))`: the plugin builds its driver from the settings, or exits with the error before the handshake, which `Load` reports.
- Built-in drivers get the same: `file.New(settings)`, `http.New(settings)`.
- A plugin restarted after a crash (PLG-18) gets the same settings.

## 5. Packages

- One package per plugin, `veho-plugin-<name>` (`.deb` and `.rpm`, amd64 and arm64, with an SBOM, as ligo's plugin packages), installing the binary and its manifest and signature under `/usr/libexec/veho/plugins/`, owned by root, mode 0755 and 0644.
- Each depends on `veho`. The packaging tool cannot pin the exact version of the dependency, so `veho` itself enforces it: a plugin whose manifest is of another release is refused at start (§3.2, step 3). `veho` and its plugins are upgraded together (`apt upgrade`, `dnf upgrade`).
- Archives (`.tar.gz`) carry the binary, the manifest and the signature.
- The packages are signed with the packaging key, as `veho`'s.
- The `veho` package installs `/etc/veho/plugin-keys/` (root, 0755) and `veho.pub` in it (root, 0644), as a plain file, not a configuration file, so that an upgrade always installs the release's current keys. A publisher's package, or the operator, adds `<publisher>.pub` next to it.

## 6. Code changes

- `cmd/veho/serve.go`: the `drivers` list; built-ins `file` and `http` only; no import of `pkg/plugin/s3`. A test checks that `veho`'s dependency graph contains no `github.com/aws/…` module.
- `pkg/plugin/process`: `Serve` with settings (§4); `Load` with the settings and the manifest verification (§3.2), in a new file `manifest.go`.
- `internal/config`: `drivers` in place of `plugins`; no backward compatibility (pre-v1, AGENTS.md rule 8).
- `cmd/veho-plugin-s3`, `cmd/veho-plugin-file` (tests only, no package): the new `Serve`.
- Tests that link `pkg/plugin/s3` directly (the engine's integration tests) keep doing so: only the `veho` binary is lean.
- The lab (`test/lab`): installs the plugin binaries of the build with `path` and `sha256`, and runs the end-to-end tests and the soak through the process loader.

## 7. Threat model

| ID | Threat | Mitigation |
| :--- | :--- | :--- |
| PD-1 | A replaced or added plugin binary runs inside veho's trust boundary (T-20). | Root-owned directory and files; the signed manifest; the SHA-256 checked by go-plugin before the start; only listed drivers start. |
| PD-2 | **Downgrade**: an older, authentic plugin with a known flaw is put back. | `veho`'s version must be in the manifest's range (§3.2, step 3): the release's plugins carry only their release. A publisher's plugin can be put back within the range the publisher signed: the publisher's responsibility, accepted (narrow ranges, a new key to revoke old manifests). |
| PD-3 | The binary is swapped between the hash check and its execution (TOCTOU). | Only root can write the directory, and root can already replace `veho` itself; accepted with R-09. |
| PD-4 | Theft of the plugin-signing key: an attacker signs a malicious plugin. | The key only in the repository secrets and the offline backup, as the packaging key (R-09); the signature also needs root to install the file (PD-1); rotation by a new key in `plugin-signing.pub`. |
| PD-5 | A plugin learns more of the platform than it needs. | Only its own `settings` and, per attempt, the credentials of the endpoint it serves (PLG-15, PLG-16). |
| PD-6 | A key added to `/etc/veho/plugin-keys/` trusts a malicious publisher. | The directory and its files must be root's and not writable by others, so adding a key needs root, which can already replace `veho`; a key signs only the drivers that name it in `veho.yaml` (§2), so an added key alone trusts nothing. |
| PD-7 | A publisher's key signs a plugin under another driver's name (`s3`), or the release key is replaced in the file. | The key is per driver and `veho` by default; `name` in the manifest must be the configured one. Replacing `veho.pub` needs root (PD-6); on hosts whose `/usr` is read-only or verified, `/etc` is not, so there the key is less protected than the binary: accepted, the same as `veho.yaml`'s. |

R-07 narrows: only `file` and `http`, which use the standard library alone, share the core's memory. A plugin process runs as the same user as `veho serve` until PLG-15's confinement comes with the packaging (backlog 27), so a malicious plugin can still read what that user reads.

## 8. Tests (written first)

| ID | Test | Kind |
| :--- | :--- | :--- |
| PD-T1 | Manifest: a valid one loads; a wrong signature, an unknown key, another name, a `veho` version outside the range, a malformed range, a different binary (SHA-256), a group- or world-writable file or directory, a key file or key directory not root's: each refused with its step | unit |
| PD-T8 | `pkg/semver`: parsing and precedence of versions (pre-releases, build metadata); ranges with each bracket, unbounded sides, a single version; malformed ranges refused; a fuzz test of `ParseRange` | unit, fuzz |
| PD-T9 | Configuration: `key` on a built-in driver or with `path` refused; a bad key name refused; a driver loads with its publisher's key and not with another's | unit |
| PD-T2 | Configuration: `drivers` names unique; `path` without `sha256` refused; an unlisted driver has no endpoints (`422` at `endpoint create`) | unit, integration |
| PD-T3 | Settings: a plugin gets its settings; a plugin that refuses them stops `Load` with its message; a restarted plugin gets the same | unit, with `cmd/veho-plugin-file` |
| PD-T4 | `veho`'s dependency graph has no `github.com/aws/` module | unit (`go list -deps`) |
| PD-T5 | `tools/plugin-manifest` signs, and `Load` verifies, with a test key | unit |
| PD-T6 | The release: the packages install the binary, the manifest and the signature where §5 says; `veho serve` loads `s3` from them | lab, on a release candidate |
| PD-T7 | The end-to-end and S3 tests of the lab pass with `s3` as a plugin | lab |

## 9. Documentation

- Setup Guide: installing the plugin packages; the `drivers` configuration; settings per driver.
- Operator's Guide: upgrading (plugins follow `veho`), a third-party plugin by `path` and `sha256`, the start-up errors of §3.2.
- Architecture and Design Guide: built-in versus plugin, the manifest and its verification, the threat rows; R-07 reworded.
- `veho-plugins.md` §6 and §7, `veho-threat-model.md` (T-20, R-07, PD-1 to PD-5), OI-10 in `veho_specification.md`.

## 10. Order of work

1. This specification: drivers configuration, settings, manifest, `s3` out of the core, packages, the lab on the process loader.
2. `veho-sources.md`: `internal/netguard`, the submission check, `http` (built in), then `sftp`, then `ftp` as plugins, then `file` with its roots.

## 11. Revision of 2026-11-25: the trusted keys on disk

The first version compiled the release's public key into `veho`, and a third-party plugin could only be pinned by its SHA-256, which the operator updates at every upgrade of the plugin. Moving the keys to disk was evaluated before the change:

- **What signing protects against is unchanged**: a wrong or older version, a corrupted or swapped file, a binary not from its publisher. Against root it protects in neither design, since root can replace `veho` with its compiled-in key as well.
- **The key file needs the same protection as the plugins**, which `veho` already checks: root's, not writable by others (PD-6).
- **Scope**: a key signs only the drivers that name it, `veho` by default, so adding a publisher's key widens nothing by itself (PD-7).
- **Cost**: one more file, from the `veho` package, without which the plugins do not start (fail closed); on hosts whose `/usr` is read-only or verified, the key in `/etc` is less protected than the binary (PD-7, accepted). One directory for all keys keeps every publisher, the release included, under the same rule.
- **Gains**: publishers sign their plugins with the same tool and format as the release; rotating veho's key needs a package, not a new binary.

Changes: `pkg/semver` (versions and ranges, public, for publishers' tools too); the manifest's `veho` range; `drivers[].key`; `process.LoadKeys`; `packaging/keys.go` removed; the `veho` package installs `veho.pub`; `tools/plugin-manifest sign -veho RANGE` (default `[<version>]`). Not added: a minimum version per driver in the configuration (PD-2 leaves that to the publisher's ranges).

