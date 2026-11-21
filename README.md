# veho

*veho* is the Latin verb *vehō*, "I carry, I convey, I transport" (from the Proto-Indo-European root *\*weǵʰ-*, "to ride"; the same family gives *vehicle* and *vector*).

Durable, asynchronous replication of large objects (typically big files) across three geographically distributed storage systems.

A client submits the URL of an object that is already available to the service (for example in an S3 bucket or an OpenStack Glance repository) together with a replication policy that names the destinations in two other regions. The node that receives the request writes the job down reliably, answers with a job ID, and the client polls that ID for progress and completion. If the node fails, another node (in the same region or in another one) resumes the job. Anything that fails is retried. Storage backends are plugins. The service is multi-tenant.

## Status

**Specification phase.** There is no implementation yet: `internal/step` and `internal/workflow` are experiments with DBOS that the implementation will replace. The specification is a draft waiting for review (`.specs/veho_specification.md`), and the work agreed for later is in `.specs/veho-backlog.md`.

The design rests on DBOS Transact for Go (v1.9.0) over a PostgreSQL cluster spread across the three regions. The experiments that checked what the design assumes about DBOS are in `.specs/veho-dbos-spike.md`, with the harness in `test/spikes/dbos`.

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
| `veho-implementation-plan.md` | The implementation chunks |
| `veho-backlog.md` | Work agreed for later |

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
