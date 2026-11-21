# replica: backlog

Work agreed for later, with what is known so far. An item here is not a specification: each needs its own spec, approved, before any code (AGENTS.md rule 1). Update this file when an item is added, started or done.

Until 1.0.0, a release does not wait for open items: each one that affects users is listed as a known limit in the README's Status section, with its number here.

| No. | Item | Recorded | Status |
| :-- | :------------------------------------ | :--------- | :---------------- |
| 1 | Review of the threat model (`replica-threat-model.md`, written 2026-11-21 as required by AGENTS.md rule 3): its open points are the retention policy, the quota catalog, the database roles and privileges, and the audit hash chain | 2026-11-21 | Draft written; waiting for review before the first implementation chunk |
| 2 | Approval of `replica_specification.md` and the topic specifications, and the answers to OI-1 to OI-10 and OI-12 | 2026-11-21 | Draft v0.1.0 waiting for review |
| 3 | WAN validation of the state store: etcd and Patroni with the real inter-region round-trip time and jitter, 24 hours (AC-18); decides between tuned etcd and the operator-approved failover fallback (`replica-architecture.md` §3.3) | 2026-11-20 | Waiting for the real RTT and jitter (OI-12); an overnight run (AGENTS.md rule 14) |
| 4 | Promote the DBOS spike (`test/spikes/dbos`) into the acceptance suite, run on every DBOS version bump (AC-21) | 2026-11-20 | Not started; do it with the first implementation chunk |
| 5 | The lab: three "regions" (separate networks), MinIO per region, Toxiproxy for latency and partitions, for AC-1 to AC-20 and the soak (AGENTS.md rules 10 and 14) | 2026-11-21 | Not started; `test/lab/` |
| 6 | Rolling upgrades with transfers in flight (AC-20): DBOS recovery is scoped by application version | 2026-11-20 | Not started; not exercised in the spike |
| 7 | Documentation pipeline and the three guides (Setup, Operator's, Architecture and Design) built from Markdown into PDF and attached to releases (AGENTS.md rule 12): `docs/build.sh` and `docs/common` as in ligo | 2026-11-21 | Not started; the structure of ligo and netcheck is the model |
| 8 | Signed releases: the configuration is ready (`.goreleaser.yaml` signs the checksums and the packages, `custom.mk` and `release.yml` require the key, `packaging/create-signing-key.sh`, the unit and post-install script); what is left is to create the packaging key on the maintainer's machine (`packaging/create-signing-key.sh`: it writes `packaging/signing-key.asc`, to be committed, and the repository secrets `GPG_PRIVATE_KEY` and `GPG_PASSPHRASE`), then test with `make snapshot` and a pre-release tag | 2026-11-21 | Prepared; waiting for the key |
| 9 | CI services: SonarQube Cloud project and Codecov for `dihedron/replica` (`sonar-project.properties` and `codecov.yml` are in place) | 2026-11-21 | Not started; needs the services enabled and `SONAR_TOKEN` |
| 10 | Replace the experimental `internal/step` and `internal/workflow` scaffold with the implementation | 2026-11-21 | Not started; after items 1 and 2 |
| 11 | Webhooks or an event stream for job completion (polling only in v1; OI-8) | 2026-11-20 | Not started |
| 12 | Out-of-process plugins over gRPC (`go-plugin`), if third parties need to write backends without recompiling (OI-10, ADR-7) | 2026-11-20 | To explore |
| 13 | Locality-aware choice of the executor of a transfer: prefer a node in the destination's region (`replica-architecture.md` §5) | 2026-11-20 | Not started; after v1 |
| 14 | Server-side copy between buckets of one backend, where the plugin advertises it (`ServerSideCopy`) | 2026-11-20 | Not started; after v1 |
| 15 | Alignment of the DBOS pin with ligo: replica is on v1.9.0, ligo on v1.8.0 | 2026-11-21 | Decide with the next ligo release |
