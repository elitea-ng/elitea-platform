# HTTP Request Node Admission (T-5D step 5d-2)

| Item | Value |
| --- | --- |
| Date | 2026-11-24 |
| Branch | `feat/5d2-http-action-node-admitted` (based on `origin/main` `2df0a49cf`, which includes 5d-1 `#2933` and 5b-2/3 `#2937`) |
| Design | `services/elitea-worker-rust/docs/http-action-node-design.md` (§4–§12) and `http-action-main-foundation-20261123.md` |
| Closes | FU-072 (end-to-end admission of `type: http`, `revision: 2` across `agent-runtime`, Worker, Main, and Web editor) |
| Components | Shared contracts & faults (`libs/proto/contracts`, `libs/go/faults`), Rust runtime (`libs/rust/agent-runtime`), Rust Worker (`services/elitea-worker-rust`), Go Main (`services/elitea-main`), Web pipeline editor (`apps/elitea-web`), and docs. |

## Business behaviour and what was not ported

Legacy EliteaUI and the legacy SDK have no dedicated `type: http` pipeline node; HTTP calls in legacy flows went through `openapi`/`postman` toolkits without a rule boundary or SSRF guard. Nothing of that legacy path was ported.

Instead, `type: http` (`revision: 2`) executes exclusively through Main's live `ExecuteHTTPAction` gRPC RPC (built in 5d-1):

1. Pipeline authors select a project-scoped rule by `operation` name (`GET /api/v2/httpactions/prompt_lib/{projectID}/rule-names`, which returns only `name` and `enabled` — never origin, URL, path template, or credentials) and bind request inputs from declared pipeline state channels (`from`) or literals (`literal`).
2. Main validates the HTTP node schema, verifies every referenced `operation` exists and is `enabled` at save time (across REST version creation/update and internal MCP version updates), and seals the `http_action_snapshot` at run start.
3. The Rust Worker binds canonical inputs from checkpointed graph state, derives deterministic `visit_id` / `effect_id` v3 (`elitea.http.effect.v3`), records durable journal intent before dispatching, calls `ExecuteHTTPAction` over mTLS with claim headers, and classifies receipts using Main's authoritative `x-elitea-http-action-retryable` gRPC response metadata header (failing closed to Class C when absent).
4. `type: http` is refused inside `Parallel` and `Map` fan-out children (direct children, saved child pipelines inside fan-out, and Agent pipeline tools inside fan-out) with a typed, readable refusal naming the node until `5d-3` adds `scope_ref` fan-out support.

## No feature flags

There is no feature flag. `HTTP_ACTION_INTEGRATION_READY` does not exist and was not added. `http` is admitted directly in `libs/proto/contracts/pipeline-node-admission-v1.json`, `libs/proto/contracts/pipeline-node-settings-v1.json`, Go Main `pipelinelimits`, Rust Worker `compiler.rs`, and Web `CompilerAdmittedNodeTypes`.

## ETL acceptance fixture compatibility

Inspected `scripts/acceptance/etl/pipelines/etl_job.yaml` (`send_webhook`) and `scripts/acceptance/etl/pipelines/silver_batch.yaml` (`enrich_fx`) without modifying either fixture:

- Both `enrich_fx` (`operation: fx_reference.rates`, `request.path`, `response`, `settings.{retry_on_fail, max_tries, wait_between_tries_ms, timeout_ms: 5000}`, `input`, `output: [fx]`) and `send_webhook` (`operation: etl_webhook.complete`, `request.{body, idempotency_key}`, `response`, `settings.{execute_once, timeout_ms: 10000, on_error: continue, always_output_data: true}`, `input`, `output`) parse, freeze, and validate identically in `agent-runtime`, Go `httpaction`/`pipelinelimits`, Worker, and Web.
- Both `etl_job.yaml` (`publish` Parallel branch `send_webhook`) and `silver_batch.yaml` (invoked as a Map child of `silver_clean`) intentionally hit the typed `5d-3` fan-out refusal naming the HTTP node until `5d-3` admits HTTP nodes inside fan-out children.

## Changed paths

- **Shared contracts & fault registry**:
  - `libs/proto/contracts/pipeline-node-admission-v1.json` (admits `"http"`, 15 compiler-admitted node families)
  - `libs/proto/contracts/pipeline-node-settings-v1.json` (adds `"http"` family with all 8 keys, `max_http_timeout_ms: 30000`, `http_timeout_range` refusal)
  - `libs/go/faults/registry/worker.json`, `libs/go/faults/codes_gen.go`, `docs/errors/README.md` (`pipeline.http.failed`, `pipeline.http.timeout`)
- **Rust runtime (`libs/rust/agent-runtime`)**:
  - `src/graph/http_action.rs`, `src/graph/http_action_tests.rs` (`HttpActionNodeDefinition` `revision: 2` with `deny_unknown_fields`, all named bounds, canonical input binding matching Go `CanonicalJSON`, `visit_id`, `effect_id_v3`, `HttpActionFrozenSnapshotV2`, `decode_receipt_v2`, reference renderer matching 25/25 vectors in `libs/jsonschema/runtime/v1/fixtures/http-action-render-vectors-v1.json`)
  - `src/graph/node_settings.rs`, `src/graph/node_settings_tests.rs` (`SettingsFamily::Http`, `MAX_HTTP_TIMEOUT_MS = 30_000`, `NodeSettingsRefusal::HttpTimeoutRange`)
- **Rust Worker (`services/elitea-worker-rust`)**:
  - `src/transport/runtime_context.rs`, `src/transport/platform_client.rs` (`execute_http_action` gRPC RPC over mTLS + claim headers, `x-elitea-http-action-retryable` header parsing failing closed to `false`, bounded receipt decode, typed `HttpActionRpcError`)
  - `src/agents/graph/http_action_runtime.rs`, `src/agents/graph/http_action_tests.rs` (`PipelineHttpActionExecutor`, `ClaimHttpActionExecutor`, `HttpActionAttempt` behind `RecoverableNode`, bounded pre-receipt 503/transport retry on identical `Invocation`, Class I vs Class C mapping, `HttpActionCommittedProjector` verifying `HTTP_RECEIPT_AUDIENCE`)
  - `src/agents/graph/{mod.rs,compiler.rs,map_compiler.rs,map_compiler_tests.rs,parallel_compiler.rs,node_settings_definition.rs,node_settings_runtime.rs,node_settings_tests.rs}` (`"http"` parse/compile arm, `AdmittedNodeFamily::Http`, `http_failure_fault`, `node_recovery_spec`/`node_result_projector`, `FanoutChildRefusal::Http`, `ToolPipelineCause::Http`)
  - `src/agents/{pipeline.rs,session.rs}` (`http_action_snapshot` extraction/validation on `PipelineExecutionProfile`, claim executor & `plan.generation` binding into `PipelineNodeRuntimes`)
- **Go Main (`services/elitea-main`)**:
  - `internal/domain/pipelinelimits/{limits.go,http_nodes.go,http_nodes_test.go,node_settings.go,node_types_test.go,node_settings_test.go}` (admits `"http"`, save-time HTTP schema validation, `HTTPNodeOperations`, `UnknownHTTPOperationRefusal`, `DisabledHTTPOperationRefusal`, 30s HTTP settings timeout cap, dedicated Parallel/Map fan-out refusal naming the node and 5d-3)
  - `internal/application/httpaction/{contract.go,node.go,service.go,frozen_test.go}` (`Receipt.Retryable` `json:"-"`, optional `input` & `settings` acceptance in `ParseNode` preserving `TemplateDigest` compatibility)
  - `internal/infra/db/repos/{http_action_effects.go,applications.go,applications_http_action_test.go}`, `internal/api/v2/mcp/internal_applications_version.go` (`rowRetryable`, save-time `CheckHTTPActionOperations` across create/update/stored-instruction/MCP version paths with zero DB queries for non-HTTP pipelines)
  - `internal/infra/storage/runtime_http_action.go` (emits `x-elitea-http-action-retryable` gRPC response metadata header)
- **Web Pipeline Editor (`apps/elitea-web`)**:
  - `src/features/pipelines/lib/flow-editor/constants/{flowEditor.constants.ts,runtimeContract.constants.ts,nodeDefaults.constants.ts,nodeDocs.constants.ts,nodeFieldInfo.constants.ts,runtimeContract.constants.test.ts}`
  - `src/features/pipelines/lib/flow-editor/helpers/node.helpers.tsx`, `src/features/pipelines/lib/{nodeSettings.helpers.ts,nodeSettings.helpers.test.ts,graphAdmission.nodes.ts,graphHttpAdmission.helpers.ts}`
  - `src/features/pipelines/ui/nodes/{HttpNode.tsx,HttpNode.test.tsx}` (Wave U2 `ExtensionNodeShell` / `NodeCardView` + `FlowNodeConfigDrawer` + Basic/Advanced sections)
  - `src/features/pipelines/ui/settings/http-action/{HttpActionSettings.tsx,HttpActionSettings.test.tsx,HttpRequestBindingsEditor.tsx,httpAction.helpers.ts,httpAction.helpers.test.ts}` (operation picker over `useListHttpActionRuleNames`, path/query/header/body/idempotency-key bindings editor, response mode/accepted statuses/max bytes, `NodeSettings` with 30s HTTP timeout cap)
  - `src/features/pipelines/ui/settings/NodeSettings.tsx`, `src/features/pipelines/ui/node-card-v2/nodeCardV2.helpers.ts`, `src/features/pipelines/ui/useFlowEditorNodeTypes.tsx`, `src/features/pipelines/ui/add-node/{nodeTypeDescriptions.ts,NodeTypeSelector.test.tsx,nodeTypeSelector.helpers.test.ts}`, `src/features/pipelines/ui/{AddNodeMenu.test.tsx,GraphExtensionsAdmission.test.tsx,ParallelAuthoringGate.test.tsx}`, `src/shared/i18n/en.json`
- **Docs**:
  - `services/elitea-worker-rust/docs/recovery-guarantees.md` (`Main x P05` & `Worker x P05` HTTP rows/footnotes)
  - `services/elitea-worker-rust/docs/source-mapping/http-action-node-admitted-20261124.md` & `README.md`

## Delivery gate properties

### Performance

- **Zero extra runtime allocations / thin pointer**: `PipelineHttpActionBinding` lives inside `PipelineExecutionContext` (`Option<Arc<...>>`) so `PipelineNodeRuntimes` and `NativePipelineApplicationParticipant` do not grow.
- **Zero overhead for non-HTTP pipelines**: `CheckHTTPActionOperations` (`repos/applications.go`) short-circuits before issuing any database query when a pipeline contains no `type: http` nodes; `http_action_snapshot` is parsed only when present in `version_details`.
- **Named bounds enforced in all layers**: inputs (256 KiB), request body (256 KiB), response `max_bytes` (2 MiB, default 1 MiB), path segment (1024 B), query names (32), headers (32 / 8192 B value), timeout (`1..=30_000` ms across both top-level and `settings.timeout_ms`), and attempts (`<= 16`).

### Durability

- **Journal before dispatch & replay without duplicate RPC**: Every HTTP node runs behind `RecoverableNode` (`http_action_runtime.rs`), which writes the durable attempt journal before dispatching and replays a completed receipt without issuing another `ExecuteHTTPAction` RPC (`durable_journal_replays_completed_result_without_duplicate_rpc`).
- **Deterministic identity across replacement**: `visit_id` binds `(thread_id, node_id, step, binding_digest)` and `effect_id_v3` binds `(execution_id, generation, activation_id, attempt, binding_digest, inputs_digest)`.
- **Owner-proof committed projection**: `HttpActionCommittedProjector` (`http_action_runtime.rs`) validates `HTTP_RECEIPT_AUDIENCE`, receipt schema, binding digest, activation id, and effect id before projecting Main-committed receipts back into graph state (`committed_projector_validates_http_receipt_audience_and_projects_result`).

### Resilience

- **Pre-effect 503/transport retry only**: `HttpActionAttempt::dispatch_with_transport_retry` retries `HttpActionRpcError::Unavailable` (503 / pool saturation / transport drop before receipt) using bounded exponential backoff clamped by `attempt_deadline` and resends the exact same `Invocation` (`attempt = 0`, same `activation_id` and `inputs_digest`), relying on Main's `Lookup`-before-`Begin` ledger (`unavailable_retries_same_invocation_before_effect_row`).
- **Effect-class discipline**: Main emits `x-elitea-http-action-retryable` (`true` only for safe methods `GET`/`HEAD`/`OPTIONS` or `replay: idempotency_key` with a bound key); Worker fails closed to `false` (Class C) when absent. Class I `uncertain`/retryable failures advance `attempt` under `settings.retry_on_fail` (`class_i_uncertain_retries_with_next_attempt_and_succeeds`); Class C `uncertain` failures never retry and record `ReplaySafety::UnknownExternalEffect { effect_id }` for operator Reconcile (`class_c_uncertain_never_retries_and_emits_reconcile_card_with_effect_id`).
- **5b `settings:` integration**: Supports `retry_on_fail`, `max_tries`, `wait_between_tries_ms`, `on_error` (`stop`/`continue`/`error_output`), `error_transition`, `always_output_data`, `execute_once`, and `timeout_ms <= 30_000`. `policy_denied` never continues even with `on_error: continue` (`policy_denied_is_never_continued_even_with_on_error_continue`).
- **Fan-out boundary**: Parallel and Map compilers refuse direct HTTP children, saved child pipelines containing HTTP nodes, and Agent pipeline tools containing HTTP nodes with a typed refusal naming the node and `5d-3` (`parallel_and_map_fanout_refuse_http_children_naming_the_node`).

### Security

- **No URL/method/credential surface in YAML or UI**: Both `agent-runtime` (`deny_unknown_fields`) and Go `httpaction`/`pipelinelimits` reject `url`, `method`, `credential`, `configuration_id`, duplicate keys/merge keys on HTTP nodes, case-insensitive duplicate header names, restricted headers (`authorization`, `proxy-authorization`, `cookie`, `set-cookie`, `x-api-key`, `api-key`, `host`, `content-length`, `content-type`, `transfer-encoding`, `connection`, `upgrade`, `te`, `trailer`, `proxy-connection`, `keep-alive`, `idempotency-key`, `x-elitea-*`, `x-forwarded-*`, `x-auth-*`, `forwarded`), Unicode line/paragraph separators (`U+0085`, `U+2028`, `U+2029`) in header values, and `revision != 2`. `HttpActionFrozenSnapshotV2::from_value` decodes `template_wire_b64` (`<= 64 KiB`) and verifies `sha256(wire) == template_digest`. The Web editor fetches only `/httpactions/prompt_lib/{projectID}/rule-names` (`name` + `enabled`) and renders no URL/origin/credential controls (`HttpActionSettings.test.tsx`).
- **Cross-language canonicalization & render parity**: Rust `agent-runtime` canonicalizes inputs and validates all 25 vectors in `libs/jsonschema/runtime/v1/fixtures/http-action-render-vectors-v1.json` byte-for-byte identically to Go `internal/application/httpaction` (`render_vectors_match`).
- **Data-free faults**: Worker maps HTTP failures to registered fault codes `pipeline.http.failed` and `pipeline.http.timeout` without echoing response bodies, headers, URLs, or secrets into errors or `node_errors`.

## Recovery-guarantee rows

Updated in `services/elitea-worker-rust/docs/recovery-guarantees.md`:

| Component × phase | Class | Enforced by | Proven by |
| --- | --- | --- | --- |
| Main × P05 HTTP action save-time admission & rule check (5d-2) | **F** typed (`400`) | `pipelinelimits.CheckPipeline` + `repos/applications.go` `CheckHTTPActionOperations` (REST + MCP version writes) | `internal/domain/pipelinelimits/...` (`http_nodes_test.go`, `node_types_test.go`, `node_settings_test.go`), `internal/infra/db/repos` (`applications_http_action_test.go`) |
| Worker × P05 HTTP Request node execution & recovery (5d-2) | **I** (safe methods or `replay: idempotency_key` + bound key) / **C** (non-idempotent writes -> Reconcile card with `effect_id`) | `RecoverableNode` + `HttpActionAttempt` + `HttpActionCommittedProjector` (`HTTP_RECEIPT_AUDIENCE`) + `x-elitea-http-action-retryable` fail-closed header | `services/elitea-worker-rust` `agents::graph::http_action_tests::*` (9 tests: GET, Class I retry, Class C uncertain reconcile, 503 same-invocation retry, journal replay, 5b settings continue/always_output_data/execute_once, policy_denied stop, 30s timeout cap, fan-out refusal, committed projector audience check) |

## `rules/security.md` category status

| Category | Status | Mechanism & Proving Test / Justification |
| --- | --- | --- |
| Trust boundaries and identity | Applicable | Worker invokes `ExecuteHTTPAction` over mTLS with claim headers (`runtime_context.rs`), Main emits `x-elitea-http-action-retryable` on the verified response (`runtime_http_action.go`), and `HttpActionCommittedProjector` enforces `HTTP_RECEIPT_AUDIENCE` (`committed_projector_validates_http_receipt_audience_and_projects_result`). |
| Authorization (object level) | Applicable | Save-time `CheckHTTPActionOperations` scopes rule lookup to the owning `project_id` across REST and internal MCP version writes (`applications.go`, `internal_applications_version.go`); Web editor queries `/httpactions/prompt_lib/{projectID}/rule-names` which exposes only rule names and enabled status (`HttpActionSettings.test.tsx`). |
| Input, parsing and amplification | Applicable | `deny_unknown_fields`, `revision == 2`, dot-segment refusal, restricted-header rejection, RFC 7230 header-value validation, bounded JSON depth/nodes, 256 KiB inputs/body cap, 2 MiB response cap, 1024 B path segment cap, `timeout_ms <= 30_000` enforced in `agent-runtime`, Go `pipelinelimits`/`httpaction`, Worker, and Web (`rejects_unknown_fields_url_method_credential_and_bad_bounds`, `inputs_size_boundary_and_dot_segments_refused`, `http_nodes_test.go`, `http_settings_timeout_above_30s_is_refused`). |
| Injection and construction | Applicable | Path/query/header canonicalization matches all 25 cross-language render vectors (`render_vectors_match`); React renders all rule names and validation messages as text nodes without raw HTML sinks (`HttpNode.test.tsx`, `HttpActionSettings.test.tsx`). |
| Egress and SSRF | Applicable | Worker makes zero direct outbound HTTP calls for `type: http`; every call traverses Main's `ExecuteHTTPAction` RPC and its 5d-1 `egress.NewHTTPActionGuard` (HTTPS-only, no proxy, no redirects, dial-time IP check). `policy_denied` receipts fail closed and cannot be bypassed via `on_error: continue` (`policy_denied_is_never_continued_even_with_on_error_continue`). |
| Secrets | Applicable | Node YAML and Web editor accept zero URLs, origins, or credentials; fault entries carry only registered codes (`pipeline.http.failed`, `pipeline.http.timeout`); diff and untracked file secret scan returned 0 hits. |
| Supply chain and deployment | Applicable | Zero new Rust, Go, or npm dependencies added. `cargo deny --all-features check advisories` passed (`advisories ok`); `govulncheck ./...` reported 0 called vulnerabilities (1 pre-existing uncalled); `npm audit --audit-level=high` reported 0 high/critical (21 pre-existing moderate dev-dep advisories). |
| Cross-layer differentials and version skew | Applicable | Single source of truth in `pipeline-node-admission-v1.json`, `pipeline-node-settings-v1.json`, and `http-action-render-vectors-v1.json` verified across Go (`pipelinelimits` parity tests), Rust (`agent-runtime` 25/25 render vectors + Worker admission/settings/fan-out parity tests), and Web (`runtimeContract.admissionParity.test.ts`, `runtimeContract.constants.test.ts`, `nodeSettings.helpers.test.ts`). |
| Agentic delegation and untrusted content isolation | Applicable | `type: http` inside `Parallel`/`Map` fan-out (including saved child pipelines and Agent pipeline tools) fails closed with a typed refusal naming the node until 5d-3 (`parallel_and_map_fanout_refuse_http_children_naming_the_node`); `scope_ref` remains `None` on Worker invocations. |
| TOCTOU, concurrency races, and effect replay | Applicable | Pre-receipt 503 retries resend the identical `Invocation` (`attempt = 0`); durable `RecoverableNode` journal prevents duplicate dispatch across crash/replay; Class C `uncertain` never retries and emits `UnknownExternalEffect { effect_id }`; Go suites verified under `-race`. |
| Sandbox escape and resource isolation | N/A | HTTP Request nodes do not execute sandboxed code or access host filesystems; artifact bodies pass only validated artifact descriptors. |
| AuthN, OAuth, PAT, and cryptographic hygiene | Applicable | Domain-separated SHA-256 digests (`elitea.http.effect.v3`, `elitea.graph.http.activation.v1`, `elitea.http.binding.v2`) computed identically in Rust and Go; no custom credentials or token handling added in Worker or Web. |

## Verification, test counts, and skips

- **Rust `libs/rust/agent-runtime`** (via `slot.sh`, `--offline --locked`):
  - `cargo fmt --all -- --check`: **PASS**
  - `cargo test --offline --locked -- graph::http_action graph::node_settings`: **28 passed, 0 failed** (includes **25/25** cross-language render vectors in `http-action-render-vectors-v1.json`)
  - `cargo clippy --offline --locked --all-targets -- -D warnings`: **PASS** (0 warnings)
- **Rust `services/elitea-worker-rust`** (via `slot.sh`, `--offline --locked`):
  - `cargo fmt --all -- --check`: **PASS**
  - `cargo test --offline --locked --lib -- agents::graph::http_action_tests agents::graph::node_admission_parity_tests agents::graph::fanout_families_parity_tests agents::graph::node_settings_tests transport::runtime_context_tests::execute_http_action`: **47 passed, 0 failed** (11 HTTP runtime/compiler/recovery/fan-out tests + 34 parity & settings tests + 2 gRPC transport tests)
  - `cargo test --offline --locked --lib -- agents::graph::`: **768 passed, 0 failed, 3 ignored**
  - `cargo clippy --offline --locked --all-targets -- -D warnings`: **PASS** (0 warnings)
  - `cargo deny --all-features check advisories`: **PASS** (`advisories ok`)
- **Go (`libs/go/faults` & `services/elitea-main`)**:
  - `libs/go/faults`: `go test ./...` & `go vet ./...`: **PASS** (all packages incl. `TestEveryRegisteredCodeIsUsed` & `TestLegacyCodesOnlyShrink`)
  - `services/elitea-main`: `GOMAXPROCS=4 go test -p 2 -race ./internal/domain/pipelinelimits/... ./internal/application/httpaction/... ./internal/infra/storage/... ./internal/infra/db/repos ./internal/api/v2/mcp/...`: **PASS** (all 9 packages)
  - `go vet ./...`: **PASS**
  - `govulncheck ./...`: **PASS** (`0` called vulnerabilities; 1 pre-existing uncalled module advisory)
- **Web (`apps/elitea-web`, Node v24.19.0)**:
  - `npx vitest run --config vitest.config.ts --project node src/features/pipelines/`: **276/276 files passed** (**3,002 passed | 1 pre-existing expected fail**)
  - `npm run typecheck`: **PASS** (0 errors)
  - `npx oxlint -c .oxlintrc.json --deny-warnings src/features/pipelines/`: **PASS** (0 warnings, 0 errors across 650 files)
  - `npm run theme-gate`: **PASS** (all 9 checks + 5 brand-pack tests)
  - `check-budgets`, `check-testid-namespace`, `check-layer-cycle`, `check-dead-code`, `i18n-backfill --check`: **ALL PASS**
  - `npm audit --audit-level=high`: **PASS** (0 high/critical; 21 pre-existing moderate dev-dep advisories)
- **Secret & feature-flag scan**: 0 secret/local-path hits across tracked diff and untracked files; 0 `HTTP_ACTION_INTEGRATION_READY` occurrences.
- **Skipped / deferred checks (disclosed)**:
  - Disposable Docker stack build/bring-up, live PostgreSQL save-time integration test execution, and live end-to-end crash-window/browser runs were not started per 5d-2 pre-review instructions and are deferred to post-review / integration verification.
  - **Expected CI visual baseline update**: Admitting `HTTP Request` increases the Add-node palette from 14 to 15 compiler-admitted node types; Linux CI visual snapshots that capture the Add-node menu or palette may require a CI baseline refresh before merge.

## Follow-ups

1. **5d-3**: Admit `type: http` inside `Parallel` and `Map` fan-out children via `scope_ref` and `execution_saved_child_scopes` (unblocks `etl_job.yaml` `publish.send_webhook` and `silver_batch.yaml` `enrich_fx` inside fan-out).
