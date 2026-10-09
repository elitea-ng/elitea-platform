# Extracting the agent runtime from the worker

ADR-0029 decision 2 moves the pure agent runtime out of
`services/elitea-worker-rust` into this crate, behind the host interface in
`src/host.rs`, so the cloud worker and the desktop host run the same code.
This file is the measured starting point and the order of the move. Each
stage lands on `main` on its own and leaves both
`services/elitea-worker-rust` (`cargo test --all-features`, Clippy) and
`libs/rust` green.

## How the move works

- A module moves with `git mv` into `src/` with its tests, and `adk_rust::`
  paths become the crates they re-export (`adk_core`, `adk_graph`,
  `adk_session`). It keeps its crate-private visibility (`pub(crate)`,
  `pub(super)`): only what the worker's production code calls becomes
  `pub` (found with the compiler), and a suite that must stay in the worker
  because it composes the module with worker code reaches it through
  fixtures behind the `test-support` feature, never through widened
  internals.
- The worker re-exports it at the old path
  (`use elitea_agent_runtime::graph::printer;` in `agents/graph/mod.rs`), so
  `super::printer::…` and `crate::agents::graph::yaml::…` call sites do not
  change.
- `#[cfg(test)]` helpers that worker tests still call are gated
  `#[cfg(any(test, feature = "test-support"))]`; the worker enables
  `test-support` for its dev build only.
- Every `serde_json::to_vec`/`to_string` on a `Value` that feeds a digest or
  rendered output becomes `canonical::to_vec`/`to_string` (below).

## Dependency map (measured 2026-10-08 on `main` @ 25cd2a50a)

Method: every `crate::…`, `super::…` and `self::…` path in `src/agents` and
`src/toolkits` (use groups expanded), resolved to a module, then closed
transitively. Cloud modules: `transport`, `state`, `sandbox`, `protocol`,
`execution`, `config`, `diagnostics`, `security`, `spool`, `bootstrap`. The
script is approximate (an item reached through a parent's re-export resolves
to the parent); the compiler is the final arbiter, and stage 1 found one such
miss (`node_recovery_definition` reaches the compiler's private
`PipelineNodeDefinition`).

Size: `agents/` 123,425 lines in 166 files, `toolkits/` 88,382 lines in 161
files (tests included). The other trees: `execution` 23,974, `transport`
22,821, `sandbox` 31,282, `protocol` 13,481, `state` 8,243.

### Direct imports of cloud modules

| From | → `protocol` | → `transport` | → `state` | → `sandbox` | → `config` | → `diagnostics` |
|---|---|---|---|---|---|---|
| `agents/` (files) | 35 | 25 | 16 | 25 | 2 | 2 |
| `toolkits/` (files) | 3 | 3 | 0 | 0 | 0 | 0 |

`agents/` → `toolkits/`: 24 files; `toolkits/` → `agents/`: 5 files
(`snapshot.rs` → `agents::request`; `direct_request.rs` → `agents::protocol`;
three test files).

### Per-module coupling (non-test code)

Direct = imports a cloud module itself; transitive = reaches one through
another runtime module.

| Module | Lines | Direct | Transitive |
|---|---|---|---|
| **Moved in stage 1** | | | |
| `graph::{yaml, printer, router, state_modifier, hitl}` | 2,457 | – | – |
| `graph::{node_recovery, node_recovery_codec}` | 1,485 | – | – |
| `graph::{turn_checkpointer, application_activation, parallel_control, code_platform_drive, http_action}` | 776 | – | – |
| `tool_namespacing` | 48 | – | – |
| **Clean, not yet moved** | | | |
| `request` | 151 | – | – |
| `context_summary` | 689 | – | – |
| `toolkits` (all but the three below) | ≈60,000 | – | – |
| **Coupled through a hub** | | | |
| `instruction_authority`, `context_budget`, `context_management`, `context_status` | 1,848 | – | all, via `agents::runtime::NativeAgentAssemblyError` |
| `graph::compiler` | 2,360 | – | all, via the node modules it composes |
| `graph::{llm, direct_tool, resume, static_pause, static_tool_pause, application, parallel*, map_*}` | ≈9,800 | – | all, via `events`, `direct_hitl`, `internal_tools`, `pipeline::*` |
| `graph::node_recovery_definition` | 240 | – | compiler (private item) |
| `replay_history` | 647 | `transport` (recovery) | `transport` |
| **Directly coupled** | | | |
| `graph::code*`, `graph::node_recovery_{runtime,owner}`, `events_code_*` | ≈5,900 | `sandbox` (+`transport`, `protocol`, `state`, `config`) | all |
| `events` | 6,302 | `protocol` (`NodeEventV1`) | all |
| `session`, `model_scope` | 3,774 | `protocol`, `state` | all |
| `runtime`, `ordinary`, `pipeline`, `native_runtime` | ≈9,900 | all five | all |
| `application_tools`, `attachments`, `attachment_context`, `internal_tools` | ≈9,600 | `protocol`, `transport` | all |
| `toolkits::families::artifact`, `toolkits::materialize` | 1,606 | `transport` (`PlatformClient`) | `protocol`, `transport` |
| `toolkits::direct_request` | 485 | `protocol` | `protocol` |

### Hubs

The modules most other modules reach the cloud through, by number of
otherwise-clean dependents: the `agents` root (16, its re-exports of
`protocol::ProtocolError` and the request types), `events` (14,
`NodeEventV1`), `pipeline::scoped_applications` (11), `graph::compiler` (10),
`runtime` (10, `NativeAgentAssemblyError`), `application_tools` (9),
`pipeline::composition` (9). Cutting a hub frees its dependents, which is
what orders the stages below.

### Digest and rendering sites still on `serde_json::to_vec`

251 `serde_json::to_vec`/`to_string` calls remain in non-test `agents/` and
`toolkits/` code. The files among them that also compute a digest, and so
must move to `canonical` with their stage:
`application_pipeline.rs` (+`boundary.rs`, `static_pause.rs`),
`application_tools.rs`, `attachment_context.rs`, `context_compaction.rs`,
`direct_hitl.rs`, `events.rs`, `events_code_debug.rs`,
`graph/{application, code, code_committed, code_debug, code_runtime,
code_trace, compiler, direct_tool, llm, map_reduce, map_yaml,
node_recovery_definition, node_recovery_receipt, node_recovery_runtime,
parallel, static_tool_pause}.rs`, `instruction_authority.rs`,
`model_checkpoint.rs`, `model_scope.rs`,
`pipeline/{composition, scope_receipts, scoped_applications}.rs`,
`session.rs`, `toolkits/{delegated_auth, direct_execution,
mcp_tool_cache}.rs`, `toolkits/families/{yagmail, zephyr_squad}/client.rs`.
A call over a typed struct (field order fixed) or a `BTreeMap` is already
order-explicit; a call over a `Value`, or a struct holding one, is not.

## The digest hazard

Without `preserve_order`, a `serde_json::Map` is a `BTreeMap` and
`to_vec` writes members sorted by key bytes; with it, in insertion order.
Cargo unifies features per build, so linking one engine crate (engine-core,
engine-sidecar, model-client, adk-gateway, repo-ingest, code-parsers,
content-source, conversation) into a binary with the runtime flips every
such digest. `canonical::to_vec` writes
the sorted form whatever the feature set is; it equals the worker's
historical output byte for byte, so no stored digest changes.

Proof in this crate:

- `canonical` tests: sorted at every depth; equal to `serde_json::to_vec`
  when `preserve_order` is off; different from it when it is on (the hazard
  is real), and the `test-preserve-order` feature does reach serde_json.
- Pinned digests computed by the pre-move code (and, for the activation
  input digest, independently with Python's
  `json.dumps(sort_keys=True, separators=(",", ":"))`):
  `printer::order_tests`, `application_activation::tests::input_digest_*`,
  `hitl::order_tests`.
- CI (`ci-deepwiki-engine.yml`) runs them three ways: inside the workspace
  (where other members link `preserve_order`), alone (off, as the worker
  links it) and alone with `--features test-preserve-order`.
- The worker's own pinned digest tests (`printer_tests`, `hitl_tests`,
  `node_recovery_*_tests`) run unchanged against the moved code.

Two neighbouring hazards the canonical encoder does not cover:

- **Number text.** The worker builds serde_json with `arbitrary_precision`,
  which keeps a parsed number's original text (`1.50`, big integers). A
  desktop build without it writes `1.5`. Durable state is per host, so this
  does not break either host, but cross-host conformance fixtures that
  compare digests over parsed numbers must use integers or normalise. The
  runtime must not require `arbitrary_precision` itself: in the libs
  workspace it would unify into engine-core's Python-compatible number
  semantics.
- **minijinja `preserve_order`.** Template maps (`router`, `state_modifier`)
  are minijinja maps, sorted unless minijinja's own `preserve_order` feature
  is unified in. No crate here enables it today; a crate that does would
  change rendered templates. Watch for it in the desktop dependency closure.

## Stages

Effort is engineer-days for one engineer who knows the worker. Every stage
keeps the worker's tests and pinned digests unchanged.

| Stage | Moves | Enabling work | Effort |
|---|---|---|---|
| **1** | Graph leaves (`yaml`, `printer`, `router`, `state_modifier`, `hitl`, `node_recovery` + codec, `turn_checkpointer`, `application_activation`, `parallel_control`, `code_platform_drive`, `http_action`), `tool_namespacing`; ≈4,800 lines + ≈1,250 test lines | Crate; host traits, with the cloud `ExecutionGuard` (`ClaimLeaseStateProbe`) and a local one; `canonical`; `test-support` / `test-preserve-order` features; CI | done |
| **2 — shared vocabulary and patched adk** | `request`, `context_summary`; `NativeAgentAssemblyError`(+code) out of `runtime.rs` with `toolkits::delegated_auth` (its payload); then `instruction_authority`, `context_budget`, `context_status`, `context_management` | Move the vendored adk patches (`vendor/adk-agent`, `vendor/adk-runner`) to `libs/rust/vendor/` and patch them in both the worker and the libs workspace, or the runtime tests run unpatched adk; `impl From<RuntimeContextError> for NativeAgentAssemblyError` stays in the worker (allowed by the orphan rule) | done (below) |
| **3 — toolkits** | `toolkits/` except `families::artifact`, `materialize`'s artifact branch and `direct_request` (≈60k lines, ≈25k test lines): families, `mcp*`, `policy`, `invocation`, `tool_binding`, `snapshot`, `direct_execution`, `direct_runtime`, `sdk_conformance` | `families::sql` behind a `toolkit-sql` feature (sqlx postgres + mysql); TLS audit: `adk-tool`'s `http-transport` and reqwest 0.13 pull `aws-lc-rs` into today's worker build, so a ring-only desktop needs their features trimmed; canonical digests in `delegated_auth`, `direct_execution`, `mcp_tool_cache`; `ToolProvider` cloud adapter over `materialize_*` | mostly done (below); TLS trim and the `ToolProvider` adapter open |
| **4 — events and input protocol** | `events` (neutral projected event type; the `NodeEventV1` encoding stays in the worker behind `EventSink`), `agents::protocol` input parsing and `result` (split `protocol/` into the neutral input schema and the gRPC wire), `toolkits::direct_request` and `toolkits::direct_runtime` (it takes a `DirectToolkitRequest`) | `EventSink` cloud adapter over the output stream; frees the 14 dependents of `events` and the `agents` root re-exports | 7–8 |
| **5 — sessions, models, state** | `session` (minus `AuthorizedNativeCommandBinding`), `model_scope`(+output), `model_checkpoint`, `runner_history`, `replay_history`, `context_compaction` | Adopt `ModelTransport`/`BoundModel` (`BoundOrdinaryAgentModel`), `StateStore` (`PostgresSessionService`, `PostgresCheckpointer`), `ExecutionGuard` in place of `StateWriterLease` in moved code; cloud adapters in the worker | 8–10 |
| **6 — graph engine** | `graph::compiler`, `llm`, `direct_tool`, `resume`, `static_pause`, `static_tool_pause`, `application`, `parallel*`, `map_*`, `node_events*`, `node_recovery_{definition,receipt,runtime,owner}`, `decision`, `agent`; `pipeline/*`, `application_pipeline/*`, `direct_hitl`, `sensitive_tools`, `variables` | `ApprovalChannel` for HITL pauses; the `CodeSandbox` cloud adapter over `sandbox::client::SandboxClient` for `graph::code*` (the trait landed with stage 3); canonical digests across the compiler, LLM, direct-tool and recovery receipts | 10–12 |
| **7 — assembly and the `cloud` feature** | `runtime`, `ordinary`, `pipeline`, `native_runtime`, `assembly`, `application_tools`, `attachments`, `attachment_context`, `attachment_tools`, `internal_tools` | `DefinitionSource` and `MemoryStore` adoption (gate 7b memory tools land once, here); the builder tools onto `PlatformWriter` (the trait and its cloud adapter landed with stage 3); the worker keeps `execution/`, `transport/`, `state/`, `spool`, `security`, `bootstrap`; one conformance fixture set run by both hosts | 8–10 |

### Stages 2 and 3 as landed

Measured on `feat/desktop-agent-runtime-2` (parent `f084c75b4`).

**Vendored adk (stage 2a).** `adk-agent`, `adk-runner` and `adk-sandbox`
2.2.0 moved from `services/elitea-worker-rust/vendor/` to
`libs/rust/vendor/` (provenance and patch notes: `vendor/README.md`). The
worker's `[patch.crates-io]` points there; `libs/rust/Cargo.toml` applies
the same patch for `adk-agent` and `adk-runner` (not `adk-sandbox`: only
the worker's `sandbox-supervisor` links it, and Cargo warns on an unused
patch). The runtime depends on both patched crates, `cargo tree -i` shows
the vendor path in both workspaces, and two `const _` items in `lib.rs`
name the patched-only builders in every build, so a host without the patch
fails to compile (`vendor/README.md`). A desktop host must carry the same
two lines.

**Shared vocabulary (stage 2).** `request`, `context_summary`,
`assembly_error` (`NativeAgentAssemblyError` + code), `instruction_authority`,
`context_budget`, `context_status`, `context_management`: 2.8k lines. The
worker re-exports each at its old path. `instruction_authority`'s unit tests
moved with it; the suites that compose the authority with internal tools,
Postgres sessions and durable compaction stay in the worker and build a
plan through `test-support` fixtures (`InstructionPlan::fixture`,
`sources_for_test`, `render_for_test`), so its internals stay private.

**Toolkits (stage 3).** 62.4k lines in 129 files: every family (the
`artifact` family too, see below), `mcp`, `mcp_error`, `mcp_tool_cache`,
`delegated_auth`, `policy`, `invocation`, `snapshot`, `tool_binding`,
`direct_execution`, `materialize`. The worker re-exports the module whole
(`pub(crate) use elitea_agent_runtime::toolkits::*`).

- `artifact` moved instead of staying: it writes through the new
  `host::PlatformWriter` (value types in `platform`), and the worker's
  `transport::platform_writer::ClaimPlatformWriter` is the cloud adapter
  over `PlatformClient` and the claim-bound authority. It maps
  `RuntimeContextError` onto `HostErrorCode` (`Rejected` → `InvalidInput`)
  and keeps the `runtime_context.*` code as the log reason
  (`HostError::with_reason`); the model-visible failure text is unchanged.
  No family depends on a claim, lease or transport type.
- `sql` is behind `toolkit-sql` (sqlx postgres + mysql), enabled by the
  worker; without it a `sql` toolkit is skipped as unsupported.
- Digests: `delegated_auth`'s persisted requirement encoding writes its
  nested metadata sorted (`serialize_sorted_metadata`, pinned with and
  without `preserve_order`; byte-identical to the worker's output), and
  `authorization_tool_name` hashes the canonical encoding of its identity
  (it hashed `json!(..).to_string()`, which followed `preserve_order`). The
  `direct_execution` and `mcp_tool_cache` sites flagged above are byte-length
  bounds, which member order cannot change: no change needed.
- `host::CodeSandbox` (submit/cancel a `CodeJob`) is defined for stage 6;
  nothing calls it yet, so it has no cloud adapter and `Host` does not
  carry one (a host would only stub it).
- The per-family behaviour suites (`toolkits/*_tests.rs`) and
  `sdk_conformance` (+ exemptions) moved here, so the families keep the
  visibility they had in the worker. The gate `include_str!`s elitea-main's
  toolkit schema snapshots; `ci-deepwiki-engine.yml` (which runs this
  crate's tests) triggers on both files.

**Still in the worker, by design or for a later stage:**

- `direct_request` and `direct_runtime` (gRPC input parsing): stage 4.
- `artifact_tests` (it composes the family with `ClaimPlatformWriter` and
  the runtime-context transport) and the SDK gate's `artifact` check, which
  it reaches through `test-support`.
- `openapi_pipeline_tests` (the pipeline half of the OpenAPI expiry suite;
  it needs the worker's graph): its fixtures are
  `families::openapi::tools::test_support`.
- The TLS trim (finding 3): the runtime now carries the `adk-tool`
  `http-transport` → reqwest 0.13 → `aws-lc-rs` edge itself, so a ring-only
  desktop build still needs those features trimmed upstream or patched.
- The `ToolProvider` cloud adapter over `materialize_*`: nothing in the
  runtime calls `ToolProvider` before assembly moves (stage 7), so the
  worker still calls `materialize_*` directly.

Total ≈ 44–54 engineer-days (9–11 weeks), inside ADR-0029's 8–12 week
estimate for the extraction. Stages 2 and 3 are independent of each other
and of 4; 5 needs 2 and 4; 6 needs 3–5; 7 needs 6.

## Findings against ADR-0029

1. **The trait set misses two capabilities.** The runtime writes to the
   platform (`PlatformClient::{write_skill, write_project_context,
   list/read/write/delete_artifact}`: the skills/project-context builder
   tools and the `artifact` toolkit family) and runs code
   (`graph::code*` over `sandbox/`). Neither is a `DefinitionSource`,
   `ToolProvider` or `ApprovalChannel`. Stage 7 needs a platform-write
   capability (or the artifact family as a remote toolkit) and stage 6 a
   code-execution trait, whose desktop implementation is decision 4's
   sandbox. *Resolved in stages 2–3:* `PlatformWriter` (with the cloud
   adapter, used by the moved `artifact` family) and `CodeSandbox` (trait
   only) are in `host.rs`.
2. **sqlx stays in the runtime.** The `sql` toolkit family uses sqlx with
   the postgres and mysql drivers to reach the user's databases. "sqlx-postgres
   behind `cloud`" holds only if that family is feature-gated; on the desktop
   it is a credentialed toolkit and goes through decision 3 anyway.
   *Resolved in stage 3:* the `toolkit-sql` feature.
3. **The worker is not ring-only today.** Its default build links
   `aws-lc-rs` through `adk-tool`'s `http-transport` (MCP) and reqwest 0.13's
   default rustls provider, besides `ring`. One-TLS-stack-ring for the
   desktop needs those features trimmed (stage 3).
4. **The vendored adk patches must move.** The agent loop and compaction run
   on the worker's patched `adk-agent`/`adk-runner` (`[patch.crates-io]` in
   the worker). A library cannot carry a patch for its consumers; the libs
   workspace and every host must apply the same patches (stage 2).
   *Resolved in stage 2:* `libs/rust/vendor/`, patched in both workspaces.
7. **Some worker suites drive runtime internals.** `instruction_authority`'s
   suite needs worker modules, so it stays there and the plan internals it
   drives became public API; the family suites read elitea-main's schema
   snapshots, so they stay too. The desktop host cannot run either: the
   "one conformance fixture set run by both hosts" of stage 7 needs them
   rewritten against the public surface.
5. **`arbitrary_precision` is a second unification hazard** (number text,
   above). The ADR names only `preserve_order`, and its list of crates that
   enable it misses `content-source` and `conversation`.
6. **The event projection is runtime logic, not transport.** Most of
   `events.rs` (6.3k lines) is the projection the desktop also needs; only
   the `NodeEventV1` encoding is the cloud's. `EventSink` therefore receives
   adk events in stage 1's shape and a neutral projected event from stage 4.
