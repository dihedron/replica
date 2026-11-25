# veho: validating an endpoint's configuration

This file specifies backlog item 39, PLG-10 of `veho-plugins.md`: an endpoint's configuration is validated when the endpoint is created or updated, so a wrong one is refused then, with a message that says why, and not at the endpoint test or the first transfer (`veho-storage-class.md` §6). It is part of `veho_specification.md` and extends the driver contract and the plugin protocol (`veho-plugins.md`).

Status: proposed, 2026-11-25; awaiting approval.

## 1. Today

- Every driver ships a JSON Schema of its configuration (`ConfigSchema()`, `schema.json` embedded), carried over the plugin protocol in `Describe`; nothing validates against it.
- `plugin.ConfigChecker` (`CheckConfig(Config) error`) lets a driver check a configuration at `endpoint create`; only `file` has one, and the plugin protocol does not carry it, so a plugin's configuration is checked at its first `Open` only.
- Each driver's `Open` decodes and checks its configuration (unknown keys refused, values checked), then authenticates or connects: the checks exist, but run too late.

## 2. Decisions (proposed)

| # | Question | Proposal |
| :--- | :--- | :--- |
| 1 | What is checked | Two layers, both at `endpoint create` and `update` (and at `veho endpoint create --dry-run`, none today: not added): first the driver's **JSON Schema** (types, required keys, enums, patterns, unknown keys), by the engine, for every driver alike; then the driver's own **`CheckConfig`**, for what a schema cannot say (a format's rules, combinations of keys, a `ca_cert` that is not PEM, an archive storage class). |
| 2 | The schema validator | `github.com/santhosh-tekuri/jsonschema/v6` (v6.0.3, 2026-06-28; draft 2020-12, the drivers' `$schema`; pure Go, no dependencies). The schemas are compiled once per driver when the node starts; a driver whose schema does not compile stops the node with the driver's name (as a plugin's bad settings do). No `$ref` outside the schema is followed (the loader resolves none: NC-1's reasoning, a schema must not make the node fetch). |
| 3 | `CheckConfig` over the plugin protocol | A new RPC `CheckConfig(Config) returns (Empty)`; `Unimplemented` from an older plugin is no error (the schema layer still runs). The process client then implements `plugin.ConfigChecker`. |
| 4 | Which drivers get a `CheckConfig` | Every shipped driver: each `Open` is split into `parse(config)` (decode, unknown keys, values: no credentials, no network) and the rest; `CheckConfig` is `parse`. `file` keeps its own. Checks that need the service (Nexus: the repository's format; Swift: the container's policy) stay in `Open`. |
| 5 | The error | `invalid_argument` (HTTP 400), as today's `file` refusal: the schema's message with the JSON path of the value (`/upload: value must be one of "put", "glance-direct", "web-download"`), or the driver's message. Never the credentials (a configuration holds none). |
| 6 | Endpoints already stored | Not revalidated: a stored endpoint that a stricter driver would now refuse keeps working until it is updated, and fails at its `Open` as today. A `veho endpoint list --invalid`, which would revalidate them, is not added. |

## 3. Threat model

| ID | Threat | Mitigation |
| :--- | :--- | :--- |
| CV-1 | **A schema makes the node fetch** (a remote `$ref`, a format that resolves a name). | No loader for remote references; formats validated as assertions without I/O; schemas compiled at start, from the driver itself. |
| CV-2 | **A hostile configuration exhausts the validator** (deep nesting, huge strings, pathological patterns). | The API's request size limit bounds the input; the schemas' patterns are the drivers' own, compiled with Go's `regexp` (RE2, linear time); `CheckConfig` runs with the request's deadline. |
| CV-3 | **A plugin's `CheckConfig` hangs or crashes.** | The call has the request's deadline; a crashed plugin is restarted as for any call; the error is the plugin's, classified `Permanent`, never a 500 with the plugin's internals. |

## 4. Tests (written first)

| ID | Test | Kind |
| :--- | :--- | :--- |
| CV-T1 | Every shipped driver's schema compiles; a valid configuration of each passes; for each, a wrong type, an unknown key and a bad enum are refused with the path of the value | unit |
| CV-T2 | Each driver's `CheckConfig`: what its `Open` refused without credentials is now refused by `CheckConfig` (one case per rule), and a valid configuration passes | unit |
| CV-T3 | `CheckConfig` over gRPC: an error crosses as the driver's message; a driver without it passes; an older plugin (`Unimplemented`) passes | unit |
| CV-T4 | `endpoint create` and `update` refuse a wrong configuration of a plugin driver (s3 with `"bucket": 3`, glance with `"upload": "ftp"`, nexus with `"format": "npm", "npm_dist_tag": 1`) with `invalid_argument` and the path; the endpoint is not stored | integration |
| CV-T5 | A schema with a remote `$ref` does not compile, and the node does not start | unit |

## 5. Documentation

The Operator's Guide (the error at `endpoint create`, its message), the Architecture and Design Guide (PLG-10 as implemented), `veho-plugins.md` (the RPC), `veho-threat-model.md` (CV-1 to CV-3, in T-39's row of plugin trust).
