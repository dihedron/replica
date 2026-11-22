# veho: release documents

**Status:** approved 2026-11-22 (requested by the maintainer, for preliminary sharing with colleagues). Requirements: AGENTS.md rule 12, backlog item 7.

## 1. Documents

Three documents, built as PDFs from Markdown and attached to every release; until 1.0.0 each says on its second page that it is preliminary.

| Document | For | Answers |
| :------------------ | :-------------------- | :------------------------------------------------------------ |
| **Setup Guide** | Whoever installs veho | What does a deployment need, how do I prepare the state store, configure and start the nodes, and check that a first replication works? |
| **Architecture and Design** | Architects, security reviewers | How does veho work, why is it built this way, what does it protect against and what does it not? |
| **Operator's Guide** | Whoever runs it, and on call | How do I watch it, what are the routine and incident procedures, what does each error mean? |

The documents are self-contained: a reader never needs `.specs/`, which stays the developers' design. Out of scope: an HTML site, translations, documentation of the code.

## 2. Toolchain

Copied from ligo (`docs/` at the time of writing):

- pandoc with LaTeX and the Eisvogel template, from the `pandoc/extra` image pinned by version and digest; Mermaid diagrams (`docs/common/diagrams/*.md`, one `mermaid` block each) rendered by the pinned `mermaid-cli` image; `docs/build.sh` runs both, with docker or podman. Output in `build/docs/` (goreleaser cleans `dist/`).
- One directory per document, one file per chapter (`NN-name.md`, in file order) and a `metadata.yaml`; `docs/common/` holds the shared defaults, the front matter, the filters, the diagrams and the artwork.
- Filters: `include.lua` shows a file in a code block; `landscape.lua` turns a `::: landscape` div into landscape pages, for a table or a diagram too wide for portrait (rule 12); `breakcode.lua` allows breaks in long inline code.
- Artwork: `docs/common/artwork/veho-symbol.svg`, a preliminary logo (a parcel carried up the two arms of a V to two regions, the WAN between them), rendered once to `logo.png` with a transparent background (LaTeX does not take SVG): `inkscape veho-symbol.svg --export-type=png --export-filename=logo.png --export-width=1400 --export-background-opacity=0`. It is on every title page. `topology.svg`, the reference deployment, is drawn by hand for the same reason Mermaid cannot place three regions side by side, and rendered once to `topology.pdf` (`inkscape topology.svg --export-type=pdf --export-filename=topology.pdf`).

## 3. Build and publication

- `make docs` builds `veho-setup-guide-<version>.pdf`, `veho-architecture-and-design-<version>.pdf` and `veho-operators-guide-<version>.pdf` into `build/docs/`; `make release` builds them with the release tag first.
- `.github/workflows/docs.yml` builds them on every change to `docs/` and attaches them to the run, so that a preliminary version can be shared from any commit.
- goreleaser attaches them to the release (`release.extra_files`) and lists them in the signed checksums file (`checksum.extra_files`). The packages do not install them.

## 4. Completeness

`docs/docs_test.go` fails when something a reader needs is missing:

- every `veho.yaml` key, read from the configuration's types, in the Setup Guide's configuration reference;
- every problem code of the API, in the Operator's Guide's error reference;
- every metric, in the Operator's Guide's monitoring chapter;
- every audit action, in the Operator's Guide's monitoring chapter;
- every top-level command of `veho`, in the Operator's Guide or the Setup Guide.

## 5. Content

The outlines fix the scope, not the wording.

- **Setup Guide**: introduction and known limits; prerequisites and sizing; getting veho; the state store; the unwrap key; configuring and starting the nodes; a first replication; upgrades and removal; configuration reference.
- **Architecture and Design**: context and goals; topology and the state store; the components of a node; jobs, transfers and the distribution policy; durable execution; tenancy, security and secrets; storage plugins; the failure model; the threat model; validation; design decisions.
- **Operator's Guide**: operating model; monitoring; routine procedures; incident procedures; error reference; troubleshooting.
