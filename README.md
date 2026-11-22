# veho

*veho* is the Latin verb *vehō*, "I carry, I convey, I transport" (from the Proto-Indo-European root *\*weǵʰ-*, "to ride"; the same family gives *vehicle* and *vector*).

Durable, asynchronous replication of large objects (typically big files) to any number of destination storage systems, wherever they are (on premises, or in other clouds: for example an S3 bucket at one provider and another provider's object store). The destinations are controlled by `veho` nodes running in three regions that share one durable control plane; the topology of the destinations has nothing to do with the three control-plane regions.

A client submits the URL of an object that is already available to the service (for example in an S3 bucket or an OpenStack Glance repository) together with a distribution policy that names the destinations (two or more, anywhere in the distribution topology) and says when the job counts as done (for example 7 of 9 copies, or 5 copies spread over 3 regions). The node that receives the request writes the job down reliably, answers with a job ID, and the client polls that ID for progress and completion. If the node fails, another node (in the same region or in another one) resumes the job. Anything that fails is retried. Storage backends are plugins. The service is multi-tenant.

## Status

**Early implementation.** Done: the distribution policy engine (`internal/policy`, `internal/topology`), the state store (`internal/store`: roles, schema, row-level security, `veho migrate`, start-up checks), the storage plugins (`pkg/plugin`: the contract, the `file` and S3 drivers, the conformance suite, and the go-plugin process loader with `cmd/veho-plugin-file` and `cmd/veho-plugin-s3`), the secrets (`internal/secrets`: envelope encryption under a versioned KEK, `veho kek`), the audit (`internal/audit`: events to the system log, forwarded to the SIEM), the engine (`internal/engine` and `veho serve`: transactional acceptance, the job and transfer workflows with fencing, retries and resume, housekeeping, metrics), the DBOS regression suite (`test/dbos`), the jobs API (`internal/api`: gRPC and REST on one TLS port; API keys, OIDC and SPIFFE identities with their entitlements; `veho tenant`, `veho apikey`, `veho entitlement`) and the lab (`test/lab`). The API also serves named policies and grants, endpoints, secrets, tenants and their administration, with rate limits and an OpenAPI 3.1 document (`api/openapi/veho.v1.yaml`). The command line client (`veho job`, `policy`, `endpoint`, `secret`, … over gRPC or REST) calls it. Next: the lab's veho nodes and the acceptance suite (C9), per `.specs/veho-implementation-plan.md`; the work agreed for later is in `.specs/veho-backlog.md`. Nothing is usable end to end yet.

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
