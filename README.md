# replica

Durable, asynchronous replication of large objects (typically big files) across three geographically distributed storage systems.

A client submits the URL of an object that is already available to the service (for example in an S3 bucket or an OpenStack Glance repository) together with a replication policy that names the destinations in two other regions. The node that receives the request writes the job down reliably, answers with a job ID, and the client polls that ID for progress and completion. If the node fails, another node (in the same region or in another one) resumes the job. Anything that fails is retried. Storage backends are plugins. The service is multi-tenant.

## Status

**Specification phase.** There is no implementation yet: `internal/step` and `internal/workflow` are experiments with DBOS that the implementation will replace. The specification is a draft waiting for review (`.specs/replica_specification.md`), and the work agreed for later is in `.specs/replica-backlog.md`.

The design rests on DBOS Transact for Go (v1.9.0) over a PostgreSQL cluster spread across the three regions. The experiments that checked what the design assumes about DBOS are in `.specs/replica-dbos-spike.md`, with the harness in `test/spikes/dbos`.

## Specifications

The project follows Spec-Driven Development (`AGENTS.md`): code starts from an approved specification in `.specs/`.

| File | Content |
| :--- | :--- |
| `replica_specification.md` | Conventions, requirements, decisions, invariants, open issues |
| `replica-architecture.md` | Topology, state store, components, failure model |
| `replica-job-lifecycle.md` | Data model, state machines, workflows, adoption of orphaned work, retries |
| `replica-api.md` | REST API and command line |
| `replica-plugins.md` | Storage plugin interfaces and conformance suite |
| `replica-tenancy.md` | Multitenancy and security |
| `replica-threat-model.md` | Threat model (STRIDE) and accepted residual risks |
| `replica-acceptance-matrix.md` | Observability, deployment, acceptance scenarios |
| `replica-dbos-spike.md` | The DBOS experiments |
| `replica-backlog.md` | Work agreed for later |

## Building

```bash
make help       # every target
make lint       # gofmt, go vet and golangci-lint (the version CI runs)
make            # lint, then build for the default platform (needs goreleaser)
go test ./...   # the tests
make secrets    # gitleaks and TruffleHog, before every commit (needs docker or podman)
make snapshot   # all platforms; signed when GPG_FINGERPRINT and GPG_KEY_FILE are set (see .specs/replica-acceptance-matrix.md §7)
make vuln       # govulncheck, before every commit
```

## Layout

```
cmd/replica/   the command line
internal/      private code
pkg/           public code (build metadata, later the plugin API)
.specs/        specifications
test/          test tooling: invariants, lab, the DBOS spike
packaging/     post-install script, service unit, public packaging key
docs/          the guides (to be written)
```

## License

MIT; see `LICENSE`.
