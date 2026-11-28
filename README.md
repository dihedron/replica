# veho

*veho* is the Latin verb *vehō*, "I carry, I convey, I transport" (from the Proto-Indo-European root *\*weǵʰ-*, "to ride"; the same family gives *vehicle* and *vector*).

Durable, asynchronous replication of large objects (typically big files) to any number of destination storage systems, wherever they are (on premises, or in other clouds: for example an S3 bucket at one provider and another provider's object store). The destinations are controlled by `veho` nodes running in three regions that share one durable control plane; the topology of the destinations has nothing to do with the three control-plane regions.

A client submits the URL of an object that is already available to the service (for example in an S3 bucket or an OpenStack Glance repository) together with a distribution policy that names the destinations (one or more, anywhere in the distribution topology) and says when the job counts as done (for example 7 of 9 copies, or 5 copies spread over 3 regions). The node that receives the request writes the job down reliably, answers with a job ID, and the client polls that ID for progress and completion. If the node fails, another node (in the same region or in another one) resumes the job. Anything that fails is retried. Storage backends are plugins. The service is multi-tenant.

## Status

**0.3.1, a preview.** veho works end to end: six nodes in three regions accept, copy, resume and verify jobs, and the acceptance scenarios of the lab pass (node and region failures, a database failover, partitions, destination outages and refusals, corrupted parts, a changed source, cancels, quotas, the distribution policy, the audit and the secrets; `test/acceptance`, run by `test/lab/lab.sh acceptance`). The Setup Guide, the Operator's Guide and Architecture and Design are attached to each release.

New in 0.3.1:

- **API calls forwarded to a node near the primary.** A node far from the database's primary sends its unary calls to a peer near it, found by the round trip each node measures every heartbeat: accepting a job costs about two round trips instead of twenty-six (p50 62 ms instead of 592 ms at a 25 ms round trip, 111 ms instead of 1170 ms at 50 ms). On with `api.advertise_url` and `api.forward.peer_ca_file`; the nodes' API certificates need `clientAuth`.
- **The latency envelope, measured and published.** Chapter 8 of the Operator's Guide charts acceptance, failover and throughput against the round trip, per commit, so every release is compared with the previous ones.
- **Authentication.** OIDC and SPIFFE callers name their tenant (`X-Veho-Tenant`, `--tenant`, `VEHO_TENANT`); names are qualified by their issuer; service accounts with rotating API keys; `veho auth login`, `logout`, `status` and step-up in the CLI; refused credentials are audited.
- **Endpoint configurations checked** against their driver's schema at create and update, plugins included; `endpoint list --invalid`.
- **Periodic network checks** of every endpoint from every node.
- **Hardened packages:** AppArmor profiles for veho and its plugins, enforced; firewalld rules.
- **The CLI's output:** `--format` (`-F`) `table|json|yaml` replaces `--output`; `--wide` lists; nested values as tables.

**Before upgrading from 0.3.0:** OIDC and SPIFFE callers must name their tenant, or get `400 tenant_required`; scripts using `--output` must use `--format`; to forward, reissue the nodes' API certificates with `extendedKeyUsage = serverAuth, clientAuth`. The schema changed in place: start from an empty database.

Known limits of this version (numbers of `.specs/veho-backlog.md`):

- No upgrade path for the schema: until a first deployment it is changed in place, and a new version may need an empty database.
- The regions and zones of the topology are created with SQL (`veho.put_location`); no command manages them yet (17).
- The audit is lightweight: events go to the system log, are not tamper-evident and can be lost on a crash (32).
- The unwrap key comes from a file or a systemd credential; Vault, HSM and Shamir unseal are planned (25, 26, 31).
- Streams (`WatchJob`) and X.509-SVID callers are not forwarded and pay every round trip to the primary (47).
- The latency envelope is measured at 25 and 50 ms only; the state store's 24-hour stability over the real WAN and an eight-hour chaos soak are still to be run (3, 18, 28).
- Packaging for Rocky/Alma has no SELinux policy yet (42).

The work agreed for later is in `.specs/veho-backlog.md`.

The design rests on DBOS Transact for Go (v1.9.0) over a PostgreSQL cluster spread across the three control-plane regions. The experiments that checked what the design assumes about DBOS are in `.specs/veho-dbos-spike.md`; they run as the regression suite `test/dbos` on every DBOS version bump.

## Verify a release

Releases are signed with the project's packaging key, [packaging/signing-key.asc](packaging/signing-key.asc), fingerprint `FB7DE97BAAFAA2A676CEE59A1B34E6A2A14CD06A`. Get the key from this repository, not from the release you are verifying.

- **The checksums file** covers every archive, package and SBOM of the release, and its signature (`.asc`) covers it:

  ```bash
  gpg --import signing-key.asc
  gpg --verify checksums.txt.asc checksums.txt
  sha256sum --check --ignore-missing checksums.txt
  ```

- **rpm packages** carry their own signature, which `rpm` and `dnf` check once the key is imported:

  ```bash
  sudo rpm --import signing-key.asc
  rpm --checksig veho_<version>_linux_amd64.rpm   # must report "digests signatures OK"
  ```

- **deb packages** carry a `debsig` origin signature. `apt` and `dpkg` do not check it on a standalone package, so verify it with `debsig-verify`, after installing its policy for the key:

  ```bash
  sudo apt-get install debsig-verify
  sudo mkdir -p /usr/share/debsig/keyrings/1B34E6A2A14CD06A /etc/debsig/policies/1B34E6A2A14CD06A
  gpg --dearmor < signing-key.asc | sudo tee /usr/share/debsig/keyrings/1B34E6A2A14CD06A/debsig.gpg > /dev/null
  sudo tee /etc/debsig/policies/1B34E6A2A14CD06A/veho.pol > /dev/null <<'POLICY'
  <?xml version="1.0"?>
  <!DOCTYPE Policy SYSTEM "https://www.debian.org/debsig/1.0/policy.dtd">
  <Policy xmlns="https://www.debian.org/debsig/1.0/">
    <Origin Name="veho" id="1B34E6A2A14CD06A" Description="veho packages"/>
    <Selection><Required Type="origin" File="debsig.gpg" id="1B34E6A2A14CD06A"/></Selection>
    <Verification MinOptional="0"><Required Type="origin" File="debsig.gpg" id="1B34E6A2A14CD06A"/></Verification>
  </Policy>
  POLICY
  debsig-verify veho_<version>_linux_amd64.deb
  ```

## Specifications

The project follows Spec-Driven Development (`AGENTS.md`): code starts from an approved specification in `.specs/`.

| File | Content |
| :--- | :--- |
| `veho_specification.md` | Conventions, requirements, decisions, invariants, open issues |
| `veho-architecture.md` | Topology, state store, components, failure model |
| `veho-job-lifecycle.md` | Data model, state machines, workflows, adoption of orphaned work, retries |
| `veho-api.md` | REST API and command line |
| `veho-plugins.md` | Storage plugin interfaces and conformance suite |
| `veho-tenancy.md` | Multitenancy and security |
| `veho-threat-model.md` | Threat model (STRIDE) and accepted residual risks |
| `veho-acceptance-matrix.md` | Observability, deployment, acceptance scenarios |
| `veho-dbos-spike.md` | The DBOS experiments |
| `veho-distribution-policy.md` | Distribution policy: destinations, M of N completion, topology spread |
| `veho-latency-envelope.md` | Supported latency and jitter, degradation, tuning |
| `veho-audit-housekeeping.md` | Audit log and housekeeping (zombies, retention, pruning) |
| `veho-store.md` | State store: database roles, schema, row-level security, migrations |
| `veho-cli.md` | The command line client (C8) |
| `veho-api-server.md` | The API server: transport, interceptors, identities, jobs (C7) |
| `veho-engine.md` | The engine: acceptance, workflows, fencing, retries, housekeeping (C6) |
| `veho-audit.md` | Lightweight audit: events to the system log and the SIEM |
| `veho-secrets.md` | Secrets: key hierarchy, envelope encryption, KEK rotation, `veho kek` |
| `veho-lab.md` | The lab: three regions in one LXD VM, emulated WAN, Patroni and etcd, Garage |
| `veho-implementation-plan.md` | The implementation chunks |
| `veho-backlog.md` | Work agreed for later |

## Testing

```bash
go test ./...                    # unit tests
test/lab/lab.sh preflight        # can this machine run the lab? (Linux, LXD 5.21+, KVM)
test/lab/lab.sh up               # three regions: etcd, Patroni, PostgreSQL 16, Garage (about 10 minutes)
test/lab/lab.sh selftest         # latency, partitions, quorum, failover, S3
test/lab/lab.sh test             # integration tests (-tags integration) against the lab
test/lab/lab.sh nodes up         # six veho nodes, two per region (lab L3)
test/lab/lab.sh nodes deploy     # build veho and the fault proxies, start the nodes
test/lab/lab.sh acceptance       # deploy, then the daytime acceptance suite (about 45 minutes)
test/lab/lab.sh reset            # back to the empty baseline in about a minute
```

The integration tests read their databases and S3 endpoints from `VEHO_TEST_*` variables (`test/lab/lab.sh env`) and skip when they are unset. See `.specs/veho-lab.md`.

## Building

```bash
make help       # every target
make lint       # gofmt, go vet and golangci-lint (the version CI runs)
make            # lint, then build for the default platform (needs goreleaser)
go test ./...   # the tests
make secrets    # gitleaks and TruffleHog, before every commit (needs docker or podman)
make snapshot   # all platforms; signed when GPG_FINGERPRINT and GPG_KEY_FILE are set (see .specs/veho-acceptance-matrix.md §7)
make vuln       # govulncheck, before every commit
```

## Layout

```
cmd/veho/   the command line
internal/      private code
pkg/           public code (build metadata, later the plugin API)
.specs/        specifications
test/          test tooling: invariants, lab, the DBOS spike
packaging/     post-install script, service unit, public packaging key
docs/          the guides (to be written)
```

## License

MIT; see `LICENSE`.
