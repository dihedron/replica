# veho: distribution of the drivers as plugins

This file specifies which storage drivers are built into `veho` and how the others are shipped, trusted, configured and started as process plugins. It revises OI-10 and is part of `veho_specification.md`; the plugin protocol itself is `veho-plugins.md` §6.

Status: approved, 2026-11-24.

## 1. Decisions (2026-11-24)

1. **Only drivers that need nothing but the Go standard library are built in**: `file` and `http` (`veho-sources.md`). Every driver with third-party dependencies is a separate binary, `veho-plugin-<name>`, started as a process plugin: `s3` (the AWS SDK, 68 packages, leaves the core), `sftp`, `ftp`, and every later one. This revises OI-10's decision of the same morning (all built-ins compiled in); `go-plugin` and its gRPC protocol are already in the core, so the rule adds no dependency to it.
2. **A plugin binary is trusted through a signed manifest**, installed with it by its package; the operator no longer copies checksums into the configuration (PLG-14's manifest, taken from backlog 8).
3. **Platform settings reach a driver through a `settings` block per driver** in veho's configuration. A plugin validates its own settings when it starts; a refusal stops `veho serve` at start, with the plugin's message.

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
  - name: acme                      # a third-party plugin, outside the release
    path: /opt/acme/veho-plugin-acme
    sha256: 9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08
```

- `name`: a built-in driver, or a plugin. For a plugin without `path`, the binary is `/usr/libexec/veho/plugins/veho-plugin-<name>` and its manifest `veho-plugin-<name>.manifest` next to it (§3).
- `path` and `sha256`: a plugin outside the release, pinned by the operator, as today. Both or neither.
- `settings`: a JSON object, passed to the driver as is: to a built-in driver's constructor, to a plugin at start (§4). Default `{}`.
- Validation: names unique; a name that is neither built in nor installed fails the configuration check, which runs before the node joins.

## 3. The signed manifest

### 3.1 Contents

```json
{"name": "s3", "version": "0.4.0", "sha256": "…64 hex…", "protocol": 1}
```

Signed with **Ed25519**: `veho-plugin-<name>.manifest` holds the JSON above, `veho-plugin-<name>.manifest.sig` the signature of its exact bytes, base64. Ed25519 is in the standard library; verifying the release's OpenPGP signature in the core would need a third-party OpenPGP library, against decision 1.

### 3.2 Verification at start

For a plugin without `path`, `veho serve`:

1. reads the manifest and its signature from the root-owned plugin directory (PLG-14: the directory and the files owned by root, not writable by others);
2. verifies the signature with the public keys compiled into `veho` (`packaging/plugin-signing.pub`, a list, so that a key can be rotated over one release);
3. checks that `name` is the configured name and **`version` is `veho`'s own version**: plugins are released with the core and only that release's plugins run, so an older signed plugin with a known flaw cannot be put back (a downgrade);
4. starts the binary through go-plugin's `SecureConfig` with the manifest's `sha256`, which hashes the file before running it, as today.

A failure at any step stops `veho serve` with the step and the plugin's name. A development build has a version of its own (a snapshot), which no release manifest carries, so it runs only plugins pinned by `path` and `sha256`.

### 3.3 Signing in the release

- A new key pair: the private key in the repository secret `PLUGIN_SIGNING_KEY` (and in the offline backup of the packaging key, R-09), the public key in `packaging/plugin-signing.pub`, compiled into `veho` with `go:embed`.
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
| PD-2 | **Downgrade**: an older, authentic plugin with a known flaw is put back. | The manifest's version must be `veho`'s own (§3.2, step 3). |
| PD-3 | The binary is swapped between the hash check and its execution (TOCTOU). | Only root can write the directory, and root can already replace `veho` itself; accepted with R-09. |
| PD-4 | Theft of the plugin-signing key: an attacker signs a malicious plugin. | The key only in the repository secrets and the offline backup, as the packaging key (R-09); the signature also needs root to install the file (PD-1); rotation by a new key in `plugin-signing.pub`. |
| PD-5 | A plugin learns more of the platform than it needs. | Only its own `settings` and, per attempt, the credentials of the endpoint it serves (PLG-15, PLG-16). |

R-07 narrows: only `file` and `http`, which use the standard library alone, share the core's memory. A plugin process runs as the same user as `veho serve` until PLG-15's confinement comes with the packaging (backlog 27), so a malicious plugin can still read what that user reads.

## 8. Tests (written first)

| ID | Test | Kind |
| :--- | :--- | :--- |
| PD-T1 | Manifest: a valid one loads; a wrong signature, an unknown key, another name, another version, a different binary (SHA-256), a group- or world-writable file or directory: each refused with its step | unit |
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
