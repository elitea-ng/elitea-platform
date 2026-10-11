# Runtime-crate log visibility (2026-10-09)

Base: `main` at `f7c6a6028`. Closes follow-up 2 of
[`pipeline-artifact-toolkit-nodes-20261009.md`](pipeline-artifact-toolkit-nodes-20261009.md) ("the materializer's
`agent_toolkit_skipped` warning comes from the `elitea_agent_runtime` crate, which the deployed `ELITEA_RUST_LOG`
filter does not show").

## What was wrong

The Worker builds its process log filter and its OTLP trace filter from one level (`ELITEA_RUST_LOG`,
`ELITEA_RUST_TRACE`) as the single directive `elitea_worker_rust={level}`. ADR-0027 moved the reusable runtime
(graph router, state modifier, HITL, printer, HTTP action, node recovery, parallel control, toolkits, context
management) into `libs/rust/agent-runtime`, crate `elitea_agent_runtime`. Every event and span that crate emits was
therefore dropped in deployed Workers, both from stdout and from trace export. Found on 2026-10-09 during the Point 5
Track B browser proof: a typed-reducer refusal warning (PR #1195, still a draft) produced no log line, and
`docker logs` showed no `elitea_agent_runtime` target at all while the Worker logged its own lifecycle failure.

The opt-in failure diagnostics (`ELITEA_RUST_FAILURE_DIAGNOSTICS=on`) had the same blind spot. Their async scope
lists only span names whose target starts with `elitea_worker_rust`, so runtime-crate spans were missing there too.

## Business behaviour

- **Taken from the current platform:** nothing. The Python indexer logs through its own logging setup. This is a
  diagnostics fix on the new platform.
- **Not ported:** the current platform's practice of logging tool arguments and payloads at debug level. The
  runtime crate's events stay limited to counts, static codes and correlation identifiers, and a test now proves it
  for representative paths.

## Changed paths

| Path | Change |
| --- | --- |
| `services/elitea-worker-rust/src/diagnostics.rs:42` | `OWNED_TARGETS = ["elitea_worker_rust", "elitea_agent_runtime"]`. The single place that names the Elitea-owned crates whose events are reviewed for field safety. |
| `services/elitea-worker-rust/src/diagnostics.rs:220-236` | `level_directive` emits `target={level}` for each owned target, comma-joined. The same level and validation as before; arbitrary directives are still refused. It feeds both the log filter (`:163`) and the trace filter (`:168`). |
| `services/elitea-worker-rust/src/diagnostics.rs:192-210` | `log_layer`: the production fmt layer and filter, factored out so the tests use exactly what the deployed Worker uses. Output is unchanged: compact, no ANSI, target and thread id, span close events, stdout. |
| `services/elitea-worker-rust/src/diagnostics/failure.rs:66` | The failure-diagnostics async scope names spans from every owned target. It still writes only name, target and `file:line`, never field values. |
| `libs/rust/agent-runtime/src/context_management.rs:139-148` | The refusal of an unrecognized context-management setting no longer logs the key. Keys are profile-authored text; the event is now `context_management_setting_refused` with no data field. |
| `services/elitea-worker-rust/src/diagnostics/runtime_log_safety_tests.rs` (new) | Log-capture test over representative runtime paths (below). |
| `services/elitea-worker-rust/src/toolkits/{mod.rs,artifact_tests.rs}` | Test-only: the artifact fixture (`FixtureRpc`, `tools_of`) is `pub(crate)` so the capture test reuses it instead of copying it. |
| `services/elitea-worker-rust/SOURCE_PARITY.md`, `docs/source-mapping/agent-runtime.md` | "Crate-scoped" now names both crates. |

`.github/workflows` is unchanged. No dependency changed (`Cargo.toml` and `Cargo.lock` untouched).

## Tests

Local: macOS arm64, Rust 1.97.1, `--offline --locked`, `f7c6a6028` plus this change.

| Test | Proves |
| --- | --- |
| `diagnostics::tests::log_filter_admits_owned_crates_at_the_configured_level_only` (`diagnostics.rs:417`) | Written first and failed on `main` (runtime warning missing). With the production layer at `warn` in a child process: runtime and Worker warnings appear; runtime and Worker info do not; `hyper` warn and `sqlx` error do not. |
| `diagnostics::tests::tracing_level_is_crate_scoped_and_rejects_directives` (`:387`) | Exact directive strings for the log and trace levels; `trace,hyper=trace` still refused for both. |
| `diagnostics::tests::every_hosted_library_crate_that_traces_is_an_owned_target` (`:468`) | Reads the Worker manifest. Every `libs/rust` path dependency (vendor excluded) that depends on `tracing` must be in `OWNED_TARGETS`, so the next extracted crate cannot silently disappear from the logs again. |
| `diagnostics::failure::tests::async_scope_names_runtime_crate_spans_but_not_dependency_spans` (`failure.rs:116`) | A runtime-crate span is named in the failure scope, a `hyper` span is not, and a span field value is never written. |
| `diagnostics::runtime_log_safety_tests::runtime_log_events_never_carry_data_values` (`runtime_log_safety_tests.rs:59`) | Production layer at `trace` (most verbose) in a child process. Drives real runtime code: router node (completed and failed), OpenAPI tool through the policy wrapper (200 and 500, sanitized error), artifact read and failed write, unsupported toolkit skip, context-management refusal. Requires 5 event names plus `outcome="completed"`, `"failed"`, `"succeeded"` and the tool wrapper's `error_code="tool.execution.unavailable"`, so an empty capture cannot pass. Asserts 14 planted markers are absent: state value, condition template text, tool argument, tool error, tool response, provider body, bearer token, toolkit name, settings secret, URL password, URL query token, settings key, conversation id. |

Child processes own a global subscriber (the PR #1172 pattern), so parallel tests cannot change callsite interest and
turn a negative assertion into a silent pass.

Mutation proof: with `OWNED_TARGETS = ["elitea_worker_rust"]`, five tests fail (the four above that concern the
runtime crate, plus the directive test). Restored afterwards.

Not driven by the capture test: the info/debug count events in `context_budget.rs`, `context_summary.rs` and
`context_management.rs:337`, the delegated-auth discovery warning, and the GitHub and SharePoint selection warnings.
Each was read: counts, static codes or a toolkit label only.

| Run | Result |
| --- | --- |
| Worker `cargo test --locked --all-targets --all-features` (the CI command) | lib 1753 passed, 0 failed, 71 ignored; every integration test file passed |
| Worker `cargo test --lib diagnostics::` | 15 passed, 1 ignored (the existing manual timing probe) |
| `libs/rust` `cargo test -p elitea-agent-runtime --all-features` | 511 passed, 0 failed |
| Worker `cargo clippy --locked --all-targets --all-features -- -D warnings` | clean |
| `cargo clippy -p elitea-agent-runtime --all-targets` with and without `toolkit-sql`, `-D warnings` | clean |
| `cargo fmt --check` (Worker, agent-runtime) | clean |
| `cargo deny --all-features check advisories` (Worker) | 1 pre-existing finding, RUSTSEC-2023-0071 (`rsa`), handled in [`dependency-advisories-20261008.md`](dependency-advisories-20261008.md). No new findings; no dependency change. |

## Performance

| Mechanism | Evidence |
| --- | --- |
| The filter is built once at startup (`diagnostics.rs:220-236`); per-event cost is the same `EnvFilter` target match as before, with one more directive. | No hot-path code changed. |
| At the default `info` level, each runtime tool call adds one span-close line (`agent.tool.invoke`) and each router node one (`agent.pipeline.router_node`), next to the Worker's own phase span closes. Warnings are rare by design. | Stack run below: one tool call added two runtime lines (one warning, one span close) plus the assembly warning. Operators who want less set `ELITEA_RUST_LOG=warn`, which still shows every runtime warning. |

## Durability

Not affected. Logging is observational: no checkpoint, journal, receipt or claim path changed.

## Resilience

| Mechanism | Evidence |
| --- | --- |
| Invalid levels still fail startup with the typed `InvalidLogLevel` / `InvalidTraceLevel` (`diagnostics.rs:220-229`). | `tracing_level_is_crate_scoped_and_rejects_directives` |
| Runtime-crate refusals (unsupported toolkit family, artifact host failure, context-management refusal) are now visible to operators instead of silent. | Stack evidence below |

## Security

| Category | Applies | How it was checked |
| --- | --- | --- |
| Secrets / data in logs | **Yes** (the point of this change) | Every tracing site in `libs/rust/agent-runtime/src` (16 sites) was read: counts, static codes and correlation identifiers only (node id, toolkit type/id/name, tool name, invocation and function-call ids), the same kinds the Worker already logs. No `%`/`?` formatting of errors, state or arguments; `.record()` writes only static codes. No `log` crate use, so nothing arrives through a log bridge. The one site that logged profile-authored text (a settings key) was changed. Proven by `runtime_log_events_never_carry_data_values` at trace level. |
| Dependency fields | Yes | Third-party and vendored (`adk_*`, `leiden_rs`) targets stay off at every level (`log_filter_admits_owned_crates_at_the_configured_level_only`). Arbitrary `RUST_LOG` directives are still ignored. |
| Trace export | Yes | The same span fields are exported over OTLP; nothing beyond the fields above. |
| Failure diagnostics | Yes | Names and `file:line` only, never field values (`async_scope_names_runtime_crate_spans_but_not_dependency_spans`). |
| Identity, authorization, input parsing, injection, egress, config secrets, supply chain | No | No route, RPC, parser, query, URL construction, outbound call, secret or dependency changed. |

`code-review` (high) on the full diff: 4 findings. Fixed: drift guard for `OWNED_TARGETS`, a tool-specific failure
assertion, a doc-comment reflow. Answered here: the info-level log volume (Performance). `security-review` on the
branch: no findings.

## Recovery guarantees

No row changes. The change is observational and touches no phase behaviour. For Worker × every phase (admission, model
call, tool call, HITL, Code, fan-out child, output delivery, settlement) the existing class in
[`../recovery-guarantees.md`](../recovery-guarantees.md) stands; what changes is that runtime-crate refusals in those
phases now reach the Worker's logs.

## Real-stack evidence

Standalone stack `elitea-logfilter`, `deploy/scripts/standalone-stack.sh` with `STANDALONE_WORKER=rust`,
`STANDALONE_HOST=logfilter.localhost`, port 18700, restored from the shared real-model dump at `main` `c0f2e5f9b`
(migrations shared 158, tenant 148). Real model `gpt-5.4-mini` through the gateway; no response mocks.

- **Image:** `ghcr.io/elitea-ng/elitea-worker-rust:runtime-log-filter`, image id `sha256:06e280f37cbb…`, built with
  `standalone-stack.sh build elitea-worker` from this change on `a2b7e67de` (rebased onto `f7c6a6028` afterwards;
  the commits in between do not touch diagnostics or the runtime crate's tracing, and the later review commit only
  changed tests and a doc comment). Every other service used the `main-c0f2e5f9b-verify` images, the shared
  verification set.
- **Image identity:** the binary was extracted with `docker create` + `docker cp` (sha256 prefix `6fbad3b703dc628f`).
  It contains the branch-only event name `context_management_setting_refused` (1 match); the
  `main-c0f2e5f9b-verify` worker binary has 0.
- **Fixtures:** agent 166 `logfilter-runtime-warn` created in the UI (Private project), model `gpt-5.4-mini`, with
  two existing toolkits attached in the UI: `attachmants` (artifact) and `aha` (a family this runtime does not
  serve).
- **With the fix:** chat 880, prompt "Read the file logfilter-missing-20261009.txt …". The model called `read_file`
  and answered "Could not read the file: no such file or bucket." `docker logs elitea-logfilter-elitea-worker-1`,
  execution `3b3e5bb5c9cfbfb01b25963b5bf74f43`:
  - `WARN … elitea_agent_runtime::toolkits::materialize: … event="agent_toolkit_skipped" reason_code="unsupported_toolkit_family" toolkit_type="aha" toolkit_id=3`
  - `WARN … elitea_agent_runtime::toolkits::families::artifact::tools: the artifact read failed event="agent_artifact_tool_failed" tool="read_file" reason_code="runtime_context.not_found"`
  - `INFO … elitea_agent_runtime::toolkits::invocation: close … toolkit_type=artifact tool_name=read_file …`
  - The requested filename does not appear anywhere in the Worker log (0 matches). No ERROR or panic lines.
- **Reload:** the page was reloaded; the turn and its answer were still shown.
- **Control (same stack, same agent, same chat):** the worker was switched to `main-c0f2e5f9b-verify` and the same
  prompt sent (file `logfilter-control-20261009.txt`). The model got the same "no such file" answer, execution
  `328d39209f6e642d7424468d88985f5c` was delivered and retired (`agent_delivery_completed`), and the Worker log had
  **0** `elitea_agent_runtime` lines.

The typed-reducer warning from the original report lives in PR #1195 (draft). Once both land it is shown by the
same filter; the drift guard keeps it that way.

## Follow-ups

- None required by this change. When another `libs/rust` crate with tracing becomes a Worker dependency,
  `every_hosted_library_crate_that_traces_is_an_owned_target` fails until it is added to `OWNED_TARGETS` and its
  log sites are reviewed for data values.
