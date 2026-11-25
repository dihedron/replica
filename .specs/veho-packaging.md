# veho: hardened packages

This file specifies the first half of backlog item 27: the confinement of `veho serve` and its plugins by the distribution's mandatory access control, and the packages' dependencies. It is part of `veho_specification.md`; it extends the threat model's T-19 and T-24 (the node's confinement, today systemd's alone: `packaging/veho.service`).

Status: proposed, 2026-11-25; awaiting approval.

## 1. Scope

| Now (27a) | Later (27b) |
| :--- | :--- |
| An **AppArmor** profile for `veho` and one for its plugins, shipped in the `.deb` and loaded at install (Ubuntu 24.04, Debian) | An **SELinux** policy module for Rocky/Alma 9 and 10, which needs an enforcing Rocky/Alma machine in the lab (the lab's containers share Ubuntu's kernel, which has no SELinux) |
| The packages' **dependencies**: `ca-certificates` (TLS to the state store, Keystone, S3), `Recommends: apparmor` | Systemd units and settings for Patroni, etcd and chrony in the Setup Guide; the OpenStack placement rules; the versions of PostgreSQL, Patroni, etcd and systemd on Rocky/Alma |
| A **firewalld** service definition (`veho`: the API port, 8443/tcp), in both packages, harmless where firewalld is absent | |

## 2. Decisions (proposed)

| # | Question | Proposal |
| :--- | :--- | :--- |
| 1 | Two profiles | `veho` for `/usr/bin/veho` and `veho-plugin` for every binary under `/usr/libexec/veho/plugins/`, entered by a `Px` transition: a plugin cannot read veho's configuration, its environment file, the unwrap key or the TLS key, only its own binary, its manifest, the system's CA roots and the network. |
| 2 | What `veho` may do | Read `/etc/veho/**` (the TLS key and `veho.env` included), `/etc/ssl/**`, `/usr/share/veho/**`, the plugins' binaries and manifests; read the systemd credentials (`/run/credentials/veho.service/*`); the network (`inet`, `inet6`, `unix`); write the system log (`/dev/log`); its own `/proc` entries. Nothing else under `/home`, `/root`, `/var` or `/tmp` (with `PrivateTmp`). |
| 3 | The `file` driver's roots | Not known to the package: the operator lists them in `/etc/apparmor.d/local/veho` (the profile includes it), as the Operator's Guide says next to `drivers[].settings.roots`; without it, a `file` endpoint fails at `Open` with a permission error the guide names. |
| 4 | Mode | **Enforce**, from the install: a profile in complain mode protects nothing. The postinstall loads it (`apparmor_parser -r`) when AppArmor is enabled, and does nothing otherwise. An operator who must can put it in complain mode (`aa-complain veho`), as the guide says. |
| 5 | Third-party plugins | Under `veho-plugin` too (any binary in the plugins' directory): a plugin that needs more (a local file, a device) gets it from the operator's `local/veho-plugin`. |

## 3. Threat model

| ID | Threat | Mitigation |
| :--- | :--- | :--- |
| PK-1 | **A compromised plugin reads veho's secrets** (the unwrap key, the TLS key, the database DSNs in `veho.env`) or other plugins' files. | `veho-plugin` denies `/etc/veho/**` and `/run/credentials/**`; it may read only the system roots, its own binary and manifest. |
| PK-2 | **A compromised `veho` reaches the rest of the host** (users' files, other services' data, writing binaries). | `veho` reads only its configuration, the roots and its plugins, writes nothing on disk except the `file` driver's roots the operator lists; no `ptrace`, no capabilities (systemd's bounding set is already empty). |
| PK-3 | **A profile too strict breaks the node** at an upgrade. | The lab runs the acceptance suite with the profiles enforced; the guide says how to read a denial (`journalctl -k`, `aa-status`) and to fall back to complain mode. |

## 4. Tests (written first)

| ID | Test | Kind |
| :--- | :--- | :--- |
| PK-T1 | Both profiles parse (`apparmor_parser -Q -T`), in CI, with the abstractions of Ubuntu 24.04 | unit (CI) |
| PK-T2 | The packages: the `.deb` carries the profiles, the firewalld service and the dependencies; the `.rpm` the firewalld service and the dependencies (`dpkg-deb -I -c`, `rpm -qpR -l` on the snapshot build) | unit (CI) |
| PK-T3 | In the lab, the profiles enforced on the six nodes: the acceptance suite's short scenarios pass (AC-1, AC-13, the network checks, a plugin started under `veho-plugin`), and `aa-status` shows both profiles enforcing on the right processes | lab |
| PK-T4 | In the lab, the confinement holds: under `veho-plugin` a read of `/etc/veho/veho.env` is denied (a probe binary placed among the plugins), under `veho` a write to `/usr/bin` is denied | lab |

## 5. Documentation

The Setup Guide (the profiles, their mode, firewalld), the Operator's Guide (the `file` driver's roots in `local/veho`, reading a denial, complain mode), the Architecture and Design Guide (the confinement, PK-1 to PK-3), `veho-threat-model.md` (T-19, T-24).
