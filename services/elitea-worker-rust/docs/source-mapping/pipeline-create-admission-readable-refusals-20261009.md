# Pipeline create admission and readable runtime refusals

Branch `fix/create-admission-readable-refusals`, 2026-10-09, from `main` `0def77b22`. Source: two findings of the
post-merge browser pass on merged `main` `0def77b22` (stack `elitea-verify-0def77b2`).

## Findings

1. **Create-path admission gap.** On `/app/pipelines/create`, a YAML with a `type: split_out` node saved as pipeline
   161 (version 187). A production Worker does not admit `split_out`: `SHAPING_INTEGRATION_READY` is
   `cfg!(feature = "graph-extensions-rehearsal")` (`src/agents/graph/compiler.rs`). Reopening it showed "This pipeline
   cannot be saved — The runtime would refuse this graph" and blocked every later save. Running it failed with the
   generic "Configuration type is not supported.".
2. **Frozen tool scope refusal is generic.** A pipeline whose direct `type: mcp` node names a toolkit not attached
   under Tools failed with "The execution input is invalid.". Only the Worker log named the cause
   (`native_agent.invalid_input`, "a pipeline direct tool node references a tool outside its frozen scope").

## Root causes

- **Web.** The graph admission gate (`GraphAdmissionGate` → `useGraphAdmission` → `judgeLivePipelineGraph`) is mounted
  only by the editor's `GeneralFormPanel`. `pages/pipelines/CreatePipeline.tsx` gated Save on `form.formState.isValid`
  alone, and that form validates only name, description and starters. The chat-surface create path
  (`processes/chat/ui/PipelineCreateForm.tsx`) hides the YAML (`showInstructions={false}`) and always stores the
  admissible starter template. So `/pipelines/create` was the only create path that could store an inadmissible
  graph.
- **Main.** The only save-time check of pipeline YAML was the size bound `pipelinelimits.Check` (512 KiB, 128 nodes).
  It runs on every pipeline write path (create, create version, update version, agent-type change, application PUT,
  import, fork, MCP instruction patch), but it never looked at node types. The server therefore accepted what the
  editor refuses.
- **Worker → Main wire.** `RuntimeErrorV1` carries only a registered `{code, safe_message, retryable}`, and Main admits
  only exact registered texts (`internal/transport/runtimegrpc/output/server.go` `runtimeFailureFrame`). The proto
  comment states that detailed causes stay with the owning service. A Worker refusal therefore cannot name a node id or
  a toolkit without breaking that contract. Main owns both the stored YAML and the frozen tool list
  (`version_details.tools[].toolkit_name`, which Main writes at start in `agentexecution/tools.go`), so Main names them.

## Business behaviour

**Current platform (reference only).** The Python SDK has no shaping nodes. An unknown node `type` fails inside the
LangGraph build at run time with a Python exception, and nothing validates it at save. A direct tool node that names an
unattached toolkit fails at run time with a toolkit lookup error. Neither behaviour is ported.

**New platform.**
- **Save (create and update), in Web and in Main.** A pipeline whose nodes declare a `type` the runtime does not run is
  refused before anything is stored.
  - Web (`/pipelines/create` as well as the editor): Save is disabled and an inline outlined alert lists each reason
    with its node: `Node split: type: "split_out" is not available on this deployment — the whole pipeline is refused.`
  - Main, on every pipeline write path: HTTP 400 `PIPELINE_NODE_TYPE_NOT_AVAILABLE`:
    `Node "split" uses the "split_out" node type, which is not available on this deployment. Remove or replace that node before saving.`
    An unknown type gets `PIPELINE_NODE_TYPE_UNSUPPORTED` (`…which the pipeline runtime does not support…`).
  - The create page shows Main's readable refusal instead of "Failed to create the pipeline.".
- **Start.** A stored pipeline that the runtime would refuse is refused before execution, with a 422 that names the
  node and the fix.
  - A non-admitted node type gets the same text as at save.
  - A direct `toolkit`/`mcp` node whose `toolkit_name` is not attached gets `PIPELINE_TOOLKIT_NOT_ATTACHED`:
    `Node "fetch_issues" uses the "github_mcp" toolkit, which is not attached to this pipeline. Attach "github_mcp" under Tools → MCP, then run the pipeline again.`
    (`Tools → Toolkit` for a `toolkit` node, matching the editor's `+ Toolkit` / `+ MCP` buttons.)
- **Runtime (Worker).** This applies only when Main's build admits more than the Worker's. A gated
  `split_out`/`aggregate` node ends with the registered, data-free message "This pipeline uses a node type that is not
  available on this deployment. Open the pipeline to see which node, then remove or replace it." (public code
  `PIPELINE_NODE_TYPE_NOT_AVAILABLE`), not "Configuration type is not supported.". An unknown type keeps the generic
  message.
- **Kept.** The frozen-scope refusal itself is a security boundary and is unchanged in the Worker. Main's check is an
  earlier, readable copy of it. It never admits anything the Worker refuses, and never refuses a toolkit the version
  attaches, including one that freezing dropped because a guardrail blocks it or its schema is unavailable (see
  Security).
- **Existing stored pipelines** (for example 161 on the reference stack) still load and list, and they accept saves
  that carry no instructions. Only writing an inadmissible graph is refused, and replacing the node is accepted.

**Deployment flag.** SplitOut and Aggregate are admitted only in graph-extensions rehearsal builds. Three compile-time
gates flip together:
- the Worker `graph-extensions-rehearsal` Cargo feature;
- the Web `VITE_GRAPH_EXTENSIONS_REHEARSAL=true`;
- the Main Go build tag `graph_extensions_rehearsal` (new, `internal/domain/pipelinelimits/shaping_*.go`).

There is no runtime registry or configuration read. No deployment sets any of the three today.

## Changes

| Component | Mechanism | Path |
|---|---|---|
| Main | Node-type admission inside the existing single save-time parse: an allow-list mirroring `parse_pipeline_node_admitting`, gated shaping types, typed 400s naming a short printable node id/type | `internal/domain/pipelinelimits/limits.go:93` (`Check`), `:109` (`check`), `:171` (`checkNodeType`), `:270` (`admission`), `:301` (`displayable`); `shaping_default.go`, `shaping_rehearsal.go` |
| Main | Start admission: `CheckStart` = `Check` + each direct tool node's `toolkit_name` against the attached tools (exact name, then the Worker's legacy key; non-ASCII left to the Worker); typed 422 | `limits.go:105` (`CheckStart`), `:197` (`checkDirectToolkit`), `:226` (`toolkitAttached`) |
| Main | Attached = frozen tools' `toolkit_name` + stored tools' `toolkit_name`/`name` (a guardrail-dropped toolkit counts as attached); no stored version or an undecodable list skips the toolkit check | `internal/application/agentexecution/http_action_snapshot.go:17`, `:36`, `:57` (`attachedToolkitNames`); `start.go:417` passes `target.SourceVersionDetails` |
| Main | One typed-refusal accessor for the start path and the route | `limits.go:65` (`Refusal`); `internal/application/agentexecution/start.go:423`; `internal/api/v2/agentexecution/route.go:597` |
| Main | Registered Worker text for a gated node type, exact match only | `internal/transport/runtimegrpc/output/server.go:1068`, `:1099` |
| Worker | `PipelineConfigurationError::NodeTypeNotAvailable(PipelineGatedNodeType)` for gated `split_out`/`aggregate`; typed cause `graph.pipeline.node_type_not_available` + static detail | `src/agents/graph/compiler.rs:2361`, `:2467-2472`, `:2948`, `:2988`, `:3005`, `:3016`; `src/agents/runtime.rs:73` |
| Worker | `RuntimeFailureKind::PipelineNodeTypeNotAvailable` → `UNSUPPORTED_CAPABILITY` + registered message | `src/execution/native_agent_lifecycle.rs:1453`; `src/protocol/output.rs:55`, `:741`, `:924`; `src/execution/toolkit_delivery_processor.rs` (`runtime_failure_code`) |
| Web | The create page judges the document Save would store with the editor's own admission, disables Save, and renders an inline outlined alert with the editor gate's title and body | `src/pages/pipelines/CreatePipeline.tsx:230`, `:293`, `:364`, `:379`; `src/pages/pipelines/ui/CreatePipelineAdmissionAlert.tsx` |
| Web | `useLivePipelineGraphAdmission(yamlCode?)` judges a caller-held document (no new barrel export; the slice stays at 20 symbols) | `src/features/pipelines/lib/livePipelineGraphAdmission.ts:123` |
| Web | The `node.type` issue names a gated type as a deployment limit | `src/features/pipelines/lib/flow-editor/constants/runtimeContract.constants.ts:104`; `src/features/pipelines/lib/graphAdmission.nodes.ts:49` |
| Web | The create failure banner shows Main's readable refusal | `CreatePipeline.tsx:376` |

No proto, migration, dependency, lockfile or CI change. Four new `t()` keys (`pages.pipelines.createPipeline.admission*`)
are backfilled into `en.json`.

## Tests

| Component | Run | Result |
|---|---|---|
| Main | `go test -race` on `domain/pipelinelimits`, `application/httpaction`, `application/agentexecution`, `api/v2/agentexecution`, `api/v2/applications`, `api/v2/eliteacore`, `api/v2/mcp`, `infra/db/repos`, `api` against a throwaway PostgreSQL 18 (`pgvector/pgvector:0.8.1-pg18-trixie`) | all 9 packages ok. After the review fix: `pipelinelimits`, `agentexecution`, `api/v2/agentexecution`, `runtimegrpc/output`, `api/v2/applications` re-run with `-race`, ok (398 tests in those 5 packages) |
| Main | `go test -tags graph_extensions_rehearsal ./internal/domain/pipelinelimits/` | ok (rehearsal build admits shaping nodes) |
| Main | `go vet`, `gofmt -l` on touched packages | clean |
| Worker | `cargo test --offline --locked` | 1,627 passed, 0 failed, 10 ignored (pre-existing). PostgreSQL-only tests skipped: `ELITEA_TEST_DATABASE_URL` unset; this change has no PostgreSQL surface in the Worker. |
| Worker | `cargo clippy --offline --locked --all-targets --all-features -- -D warnings`; `cargo fmt --all -- --check` | clean (one `doc_markdown` nit fixed before commit) |
| Web | vitest `features/pipelines`, `pages/pipelines`, `features/agents`, `shared/lib`, `processes/chat` | 460 files, 4,680 passed, 1 expected-fail (pre-existing), 1 failed: `PipelineTestChat.test.tsx` "displays the pipeline model before creating a test conversation". That file is untouched; the test passes in isolation (2 of 2 runs). It is a load-sensitive flake. |
| Web | `tsc --noEmit`; `oxlint --deny-warnings`; `scripts/check-budgets.mjs`; `scripts/i18n-backfill.mjs --check` | clean |

New tests, and what they proved red first (each was run against the original code with the new code reverted):
- **Main unit** (`internal/domain/pipelinelimits/node_types_test.go`, `node_types_production_test.go`,
  `node_types_rehearsal_test.go`, `start_test.go`; 14 tests + 2 benchmarks). Coverage:
  - every compiler-admitted type is admitted;
  - `split_out` and `aggregate` are refused on a production build (also through an alias), and admitted under the
    rehearsal tag;
  - unknown types are refused;
  - non-string, missing or nested `type`, and second YAML documents, are left to the compiler;
  - echoed values are bounded and printable (a 154-byte secret-like id is never echoed);
  - the node count is checked first;
  - `CheckStart`: unattached `mcp`/`toolkit` nodes are refused naming the panel; exact, legacy-key, internal-MCP and
    non-ASCII names are admitted; unjudgeable shapes are left to the Worker; canvas order; save ignores attachment.

  4 tests failed against the original `Check`.
- **Main PostgreSQL** (`internal/api/v2/applications/pipeline_node_type_admission_postgres_integration_test.go`, 4).
  Coverage: create (no application or version row left), create version, update version (4 variants; a rename sent
  with a refused graph is not applied), application PUT, agent→pipeline type change, and an existing inadmissible
  version that stays readable and becomes fixable. 3 of the 4 failed against the original `Check`; the fourth is a
  regression guard.
- **Main start/route**:
  - `TestCurrentPipelineStartNamesTheNodeTheRuntimeWouldRefuse`;
  - `TestCurrentPipelineStartBindsDirectToolNodesToTheAttachedTools` (6 cases, including "dropped by freezing" and
    "no stored version");
  - two new cases in `TestCurrentApplicationStartRouteNamesThePipelineLimitItRefused`.

  All failed against the original wiring.
- **Main output server**: `TestNodeTypeNotAvailableFailureAdmitsOnlyRegisteredMessage`. Exact text and code only;
  tampered text and the wrong code are rejected.
- **Worker** (written after the implementation; their new symbols do not compile against the old code):
  - `shaping_nodes_are_refused_as_not_available_while_the_gate_is_off` replaces the old assertion that pinned
    `graph.pipeline.unsupported_capability`;
  - `production_builds_refuse_shaping_yaml_naming_the_gated_type`;
  - `a_gated_node_type_ends_as_the_registered_deployment_message` (compiler → assembly → `assembly_failure` → policy;
    the message carries no node data, and `custom` stays generic);
  - the new row in `tests/agent_output_contract.rs`.
- **Web**:
  - `CreatePipeline.test.tsx`: "blocks Save with an inline reason…" and "shows the server's readable refusal…",
    both failing against the original page;
  - the existing "stores the author's own graph…" test was rewritten, because it stored an inadmissible graph, which
    is exactly the defect;
  - `GraphExtensionsRehearsal.test.tsx` asserts the deployment-limit wording and `isDeploymentGatedNodeType` for both
    flag values.

## Performance

**Budget.**
- Save: no added I/O, round trip, query, transaction or YAML parse; the node-type check runs inside the parse `Check`
  already did.
- Start: one added bounded parse of the in-bound pipeline YAML (≤ 512 KiB), next to the existing HTTP-node pre-scan
  parse, plus one JSON decode of the frozen and stored `tools` lists, which the start admission already bounds.

`go test -bench` at the worst in-bound shape (128 direct MCP nodes, padded to 512 KiB, 64 attached toolkits):

| Function | Time | Memory | Allocations | Note |
|---|---|---|---|---|
| `Check` on `main` | 10.5 ms | 3.92 MB | 6,747 | baseline |
| `Check`, this branch | 4.3 ms | 3.95 MB | 7,001 | +0.8% bytes, +3.8% allocs |
| `CheckStart`, this branch | 4.1 ms | 4.02 MB | 11,289 | — |

Wall time is noisy on the shared build host and dominated by the YAML parse. Bytes and allocations are the stable
measure. A typical pipeline (a few KiB) costs microseconds.

Further costs:
- Toolkit matching is O(nodes × attached), at most 128 × the attached tool count, on strings already in memory.
- Worker: two extra match arms on a refusal path, with no allocation.
- Web: one `judgeLivePipelineGraph` per change of the instructions string (`useMemo`), the same pass the editor already
  runs per document.

Follow-up 4 records sharing the start-time parse with the HTTP pre-scan.

## Durability

**Threats.** A partial write on refusal; a stored graph that a later save cannot reproduce; a start that admits work
and then fails.

- A refused create writes nothing: it runs in one transaction, and the test asserts no application or version row
  (browser: 0 rows after the refused create and the refused direct POST).
- A refused version update writes no field. The check runs before the `SET`. The test asserts that a rename sent with
  the refused graph is not applied (browser: version 187 keeps name `base` after a refused PUT). A refused application
  PUT is refused before any write.
- Stored versions are never rewritten. Pre-fix rows stay readable and editable in their other fields, and become
  fixable.
- A refused start admits nothing: no execution, command or claim. The refusal is a 422 at the route, before execution
  admission, in the same position as the existing size refusals (browser: the chat is empty after reload, which is
  unchanged behaviour for start refusals).
- No migration, checkpoint, digest, lease or replay path changed. The Worker refusal stays at assembly, before any
  model call, tool call or checkpoint.

### Recovery guarantee rows

| Component × phase | Class | Enforcing code | Proof |
|---|---|---|---|
| Main × admission (save, create and update) | **F**: typed 400 naming node and type; nothing stored | `pipelinelimits/limits.go:93,171`; existing callers (`repos/applications.go`, `applications/handler.go`, `eliteacore/handler.go`, `mcp/internal_applications_version.go`) | PostgreSQL handler tests; browser cases A, A2, E |
| Main × admission (start) | **F**: typed 422 naming node and type, or node and toolkit with the fix (was a generic F from the Worker); nothing admitted | `limits.go:105,197`; `http_action_snapshot.go:36`; `start.go:423`; `route.go:597` | start and route tests; browser cases B, C |
| Worker × admission (compile) | **F**: registered, data-free deployment message (was a generic F); unchanged **R** for admitted pipelines | `compiler.rs:2467`; `runtime.rs:73`; `native_agent_lifecycle.rs:1453`; `output.rs:741` | `a_gated_node_type_ends_as_the_registered_deployment_message`; Main `TestNodeTypeNotAvailableFailureAdmitsOnlyRegisteredMessage` (not reachable in the browser: Main refuses first on a matching build) |
| Web/browser × create | No durable state; a refused create stores nothing; reload shows a clean draft | `CreatePipeline.tsx:230,364` | Web tests; browser case A with reload |
| Sandbox supervisor, NATS, PostgreSQL, LLM gateway | Not touched: no Code node, bus, schema or model path changed | — | — |

No **L** row is introduced. The refusals are **F** because the graph itself is invalid for this deployment: no retry,
resume or reconcile can run it until the author changes it.

## Resilience

- **Bounded.** The existing 512 KiB and 128-node bounds run first. Echoed identifiers are at most 128 bytes of valid,
  printable UTF-8 without `"`; anything else is named generically ("A pipeline node", "it").
- **Typed and readable.** One stable code per refusal (`PIPELINE_NODE_TYPE_NOT_AVAILABLE`,
  `PIPELINE_NODE_TYPE_UNSUPPORTED`, `PIPELINE_TOOLKIT_NOT_ATTACHED`, plus the existing size codes), and each message
  names the remedy.
- **No silent coercion.** A non-string `type` or `toolkit_name`, a missing field, an undecodable tool list or a missing
  stored version is never guessed at. It is left to the Worker, which refuses it.
- **No over-refusal.**
  - Main admits every type the Worker compiler has an arm for, including `map` and `parallel`, which the Web editor
    does not offer.
  - Toolkit matching follows the Worker's exact-then-legacy-key rule and treats any non-ASCII name as attached, because
    Go and Rust lower-case non-ASCII differently.
  - A toolkit that freezing dropped (guardrail-blocked, or schema unavailable) counts as attached through its stored
    names. This was found by the code review: before the fix it was refused as "not attached".

## Security

**Category checklist** (`rules/security.md`):
- **Trust boundaries / identity:** not touched.
- **Authorization:** every check runs where the size bound already ran, behind the route's project permission
  (`router_pipeline_limit_authz_test.go` proves that ordering for the shared function). The refusal is computed only
  from the caller's own submitted text (save) or from the authorized start target (start), so no foreign tenant's
  data is involved.
- **Input/amplification:** the byte bound runs before the parse and the node count before iteration; aliases are
  followed one level, as before; the work per node is O(1).
- **Injection/construction:** no SQL, template, shell, URL or path construction is added. Echoed identifiers go into
  JSON bodies only (no headers), and CR, LF and control characters are excluded. React renders them as text (no
  `dangerouslySetInnerHTML`).
- **Egress:** none.
- **Secrets:** none added; the diff was secret-scanned before every commit. The refusals carry only the author's own
  short identifiers; tests embed a secret-like value and assert it is not echoed.
- **Supply chain:** no dependency changed.

Further points:
- **Wire contract.** `RuntimeErrorV1` still carries only registered texts. The new Worker message is data-free and
  admitted by exact match under one code. Tampered text and a wrong code are rejected (test).
- **The security boundary stays in the Worker.** `validate_tool_snapshot` is unchanged. Main's check is fail-open
  wherever it cannot judge exactly, so it can only refuse earlier, never admit more.
- **Review results.**
  - `security-review` (2026-10-09): no finding at or above the confidence bar.
  - `code-review` (high, 2026-10-09): 5 findings.
    - Fixed: guardrail-dropped toolkit misreported as not attached; a stale comment; a per-node map allocation; the
      missing test.
    - Kept as a product decision: import/fork now refuses legacy unknown node types (follow-up 1).

**Dependency audits.** No dependency changed; every finding is pre-existing on `main`.
- `govulncheck`: 19 reachable vulnerabilities from the Go 1.26.5 standard library, some also found in
  `golang.org/x/net@v0.58.0`. IDs: GO-2026-6617, -6613, -6612, -6611, -6610, -6609, -6608, -6607, -6605, -6603,
  -6600, -6599, -6218, -6091, -6090, -6089, -6088, -5972, -5026. Plus 2 imported-package and 3 required-module
  findings that are not called.
- `cargo deny --all-features check advisories`: RUSTSEC-2023-0071 (`rsa` via `sqlx-mysql`).
- `npm audit`: 7 findings (2 low, 1 moderate, 4 high: `braces`, `katex`, `smol-toml`, `source-map-js`); `package.json`
  and lockfile unchanged.
- No CI change.

## Browser evidence

**Stack.** A standalone stack of my own, `elitea-admission-fix`, built per
`handoffs/real-model-stack/README.md`.
- Endpoints: browser `http://localhost:18420` and, after a cookie collision, `http://admission.localhost:18420`;
  OIDC mock `oidc.localhost:19620`, signed in as `admin@centry.user`.
- Database: the real-model dump `product-real-models-main-0def77b22.dump`, restored into an empty database (shared
  0157, tenant 0148; no unmerged migration). The shared reference stack `elitea-verify-0def77b2` was not touched.
- Images:
  - unchanged services: the reference stack's `main-0def77b22-verify` images, built from merged `main`;
  - Main, Web and Worker: built from this branch.

| Role | Image | Source |
|---|---|---|
| Main | `ghcr.io/elitea-ng/elitea-main:admission-fix` `sha256:e7ba36049893…10d9` | `ae2d4c265` (branch head incl. review fix) |
| Web | `ghcr.io/elitea-ng/elitea-web:admission-fix` `sha256:0628c73036be…6a99` | `9d5b9cf3c` (Web content identical at head) |
| Worker | `ghcr.io/elitea-ng/elitea-worker-rust:admission-fix` `sha256:6c1b661c49bd…817a` | `eec1d0cf1` (Worker content identical at head); branch contains #1160 (`524165ed09`) |

**Binary checks before use** (`docker create` + `docker cp` + `grep -a`, never run):
- Main contains `PIPELINE_TOOLKIT_NOT_ATTACHED`, `PIPELINE_NODE_TYPE_NOT_AVAILABLE`, `not attached to this pipeline`,
  the registered Worker text, and `attachedToolkitNames` (the review fix).
- Worker contains `graph.pipeline.node_type_not_available` and the registered message.
- Web: the bundle contains `create-pipeline-admission` (`create-*.js`) and "is not available on this deployment"
  (`livePipelineGraphAdmission-*.js`).
- None of these strings exist on `main`.

**Cookie collision.** The reference stack on `localhost:18120` and mine both set the `elitea_session` cookie for host
`localhost`. Cookies are not port-scoped, so sessions kept turning `session_unknown`. Main was recreated on the same
image with `OIDC_REDIRECT_URI=http://admission.localhost:18420/auth/oidc/callback`, and cases D and D2 ran on that host.

| Case | Pipeline / chat | Result |
|---|---|---|
| A. Create page, `split_out` graph typed into the YAML editor | — | An inline outlined alert reads "This pipeline cannot be saved … Node split: type: "split_out" is not available on this deployment — the whole pipeline is refused."; Save is `disabled`; no create request was sent. After reload: a clean starter draft and no alert. |
| A2. The same create POST sent directly from the signed-in page (bypassing the Web gate) | — | 400 `Node "split" uses the "split_out" node type, which is not available on this deployment. Remove or replace that node before saving.`; 0 application rows and 0 `split_out` versions stored. |
| C. Pipeline created in the UI with a direct `mcp` node, `toolkit_name: github_mcp`, not attached | 161 / chat 867 | Save is allowed (attachment is a start-time property). The run returns 422; the chat shows `Node "fetch_issues" uses the "github_mcp" toolkit, which is not attached to this pipeline. Attach "github_mcp" under Tools → MCP, then run the pipeline again.` (was "The execution input is invalid."). The editor's TOOLS section shows the `+ MCP` button the text names. After reload the chat is empty (a refused start admits nothing). |
| C2. Positive control: existing pipeline 134, whose direct MCP node's toolkit (95, "zero argument gate5") is attached | 134 / chat 868 | Main admits (200). The Worker passes `validate_tool_snapshot`, then ends with "A required runtime dependency is unavailable." (`the native MCP toolsets could not be materialized`): that toolkit's MCP server is not reachable from this stack, so this is environmental. No false refusal. |
| B. A pipeline stored before the fix with a `split_out` node | 162 (version 187) / chat 869 | The editor shows the existing gate ("See the errors on node split"; node issue "…not available on this deployment…"). The run returns 422; the chat shows `Node "split" uses the "split_out" node type, which is not available on this deployment. …` (was "Configuration type is not supported."). Main log: `pipeline refused by start admission`. |
| E. An update PUT of version 187 with an `aggregate` node and a rename, sent from the page | 162 (187) | 400 `Node "tail" uses the "aggregate" node type, which is not available on this deployment. …`; name and instructions unchanged. |
| D. Fix in the editor: the Yaml tab is replaced by a Printer node; the gate lifts; Save | 162 (187) / chat 869 | Stored. The run answers `fixed: admission ok`; the same after reload. A first attempt with a mistyped Printer field (`printer_output`) was refused by the Worker as "The execution input is invalid.". That is correct and unrelated: the editor's mirror does not cover that field. |

**Fixtures.**
- Pipelines 161 and 162, and their runs, were created through the browser.
- Pipeline 162 was created through Main's API from the signed-in page with a valid Printer graph. Its version 187 was
  then overwritten by SQL with the `split_out` graph, to reproduce a row stored before this fix. Neither the UI nor
  the API can create one any more.
- Pipeline 134 is an existing record of the restored dump.
- No response was mocked. Only the stack's default model fixture was involved: no case calls a model.

The stack and the throwaway test PostgreSQL were torn down after the evidence was recorded.

## Open limits and follow-ups

1. **Import and fork of legacy node types (product decision).** `pipelineLimitRefusal` (import, fork) now refuses a
   pipeline with any type the compiler cannot run (for example a current-platform `function` node), so the whole
   import fails. Before, it imported an unrunnable version that could be fixed in the editor. This is kept
   fail-closed for consistency with create and update. The alternative is to exempt import and fork.
2. **Saved-child and fan-out child pipelines.** These start in the Worker, not through Main's start route, so a gated
   node there still reaches the Worker's refusal. The Worker refusal now reads as the registered deployment message,
   but it does not name the node.
3. **Other frozen-scope causes** (wrong kind, legacy-key ambiguity, `selected_tools` exclusion, a toolkit dropped by a
   guardrail) still read "The execution input is invalid.". They need distinct registered Worker messages, as
   follow-up 1 of the numeric-ids mapping did for identifiers.
4. **One start-time parse.** `declaresHTTPNode` (HTTP pre-scan) and `CheckStart` each parse the YAML. A shared scan
   would remove about 4 MB of allocation at the 512 KiB worst case.
5. **Start wording.** The start-time node-type refusal reuses the save text ("…before saving"), as the size refusals
   do (numeric-ids follow-up 8).
6. **Web mirror coverage.** The editor's admission does not check Printer field names (case D's first attempt).
   That is pre-existing.
7. **Main has no `custom`/legacy alias handling.** It mirrors the Worker exactly. The three rehearsal flags must flip
   together; nothing enforces that across services at run time.
