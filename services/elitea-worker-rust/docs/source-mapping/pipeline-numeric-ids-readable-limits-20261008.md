# Numeric node ids and readable pipeline limits

Branch `feat/pipeline-numeric-ids-readable-limits`, 2026-10-08. Follows up "Open limits and follow-ups" 1, 3 and 4 of
[pipeline-size-bounds-http-snapshot-20261008.md](pipeline-size-bounds-http-snapshot-20261008.md) (#1140). Brief:
`.claude/handoffs/point5-wave1-20261008/00-COMMON.md` plus the user task of 2026-10-08.

User decisions (2026-10-08): **support** unquoted numeric node ids; keep the **512 KiB** pipeline bound; keep the
per-node 64 KiB caps unchanged for now.

## Business behaviour

**Current platform (reference only).** `elitea_sdk/runtime/langchain/langraph_agent.py:1246` loads the pipeline with
`yaml.safe_load`, so `id: 1` is a Python `int`. `clean_string` (`elitea_sdk/runtime/utils/utils.py:72-74`) calls
`re.sub` on it and raises `TypeError` at `langraph_agent.py:1272` (node id), `:1264-1265` (interrupt lists, outside any
`try`) and `:1687` (`entry_point`, only `KeyError` is caught). Nothing in the Pylon plugins str-coerces ids first.
Numeric ids therefore **crash** the current platform. Supporting them is a deliberate improvement, not a port.

**New platform.**
- An integer YAML scalar in a graph-identifier position is its canonical decimal string: `id: 1` and `id: "1"` are
  one pipeline with one definition digest. Only integers within the JavaScript safe range ±(2^53−1) are admitted, so
  the Worker and the Web editor print the same string.
- Floats, bools, sequences, mappings, tagged values and out-of-range integers in those positions are refused with a
  typed cause naming the field. `null` keeps its previous outcome everywhere.
- Pipeline size refusals name the limit:
  - on save: HTTP 400 "The pipeline definition exceeds the 512 KiB size limit. Reduce the YAML before saving." or
    "The pipeline has more than 128 nodes. Split it into smaller pipelines before saving.";
  - on start: HTTP 422 `PIPELINE_INSTRUCTIONS_TOO_LARGE` / `PIPELINE_TOO_MANY_NODES` with the same message;
  - in the Worker: the already-registered agent-settings input-limit message ("The request cannot start because the
    agent instructions or settings exceed a platform input limit. …") instead of "The execution input is invalid.".
- Existing over-bound versions (local version 133, 8,000,358 bytes) still load, list and export. Only writes are
  refused.

**Not ported.** The Python crash; unbounded acceptance; the legacy YAML 1.1 integer spellings (`010` = 8,
`1_000` = 1000), which the YAML 1.2 parsers of the new platform read as strings.

## Changes

| Component | Mechanism | Path |
|---|---|---|
| Worker | Integer → canonical string before typed deserialization, in every identifier position: `entry_point`, `interrupt_before[]`, `interrupt_after[]`, node `id`, `transition`, `default_output`, decision `nodes[]`, router `routes[]`, hitl `routes.*`, map `worker`, parallel `branches[].id`/`.node`, and `recovery.on_failure.route` (found missing from the original list) | `src/agents/graph/compiler.rs:1655` (`normalize_graph_identifiers`), `:1834` (`normalize_graph_identifier`), `:1868` (`safe_integer_identifier`), `:1623` (`MAX_SAFE_INTEGER_IDENTIFIER`), `:1791` (recovery route) |
| Worker | Typed causes `LimitExceeded(PipelineLimit)` (`graph.pipeline.yaml_bytes_exceeded`, `…node_count_exceeded`, `…node_limit_exceeded` + family) and `InvalidIdentifier(PipelineIdentifierField)` (`graph.pipeline.invalid_identifier` + field); YAML bytes checked before parsing, node count on the parsed document before the typed parse | `compiler.rs:654-680`, `:2572`, `:2616`, `:2671`, per-node mapping `:2157-2244` |
| Worker | One mapping `NativeAgentAssemblyError::from_pipeline_configuration`; new `InputLimit(InputLimitField)` code; data-free `NativeAgentAssemblyCause` | `src/agents/runtime.rs:64`, `:90`, `:168`; callers `src/agents/pipeline.rs:2189`, `src/agents/session.rs:3119` |
| Worker | `InputLimit(field)` → `RuntimeFailureKind::ExecutionInputFieldLimit(field)`; lifecycle log carries `cause_code`/`cause_detail` (static strings) | `src/execution/native_agent_lifecycle.rs:1459`, `:292` |
| Worker | Profile byte bound names the limit instead of `invalid_profile` | `src/agents/assembly.rs:1154` |
| Main | Single source of truth `MaxInstructionsBytes = 512 KiB`, `MaxNodes = 128`, typed `ErrInstructionsTooLarge`/`ErrTooManyNodes`, `Check` (bytes first, then a bounded first-document parse counting top-level `nodes`; unparseable YAML is not refused) | `internal/domain/pipelinelimits/limits.go:20`, `:51` |
| Main | Save-time bound on every pipeline write: create / create version (`insertVersion`), update version, agent-type change to `pipeline` (stored text checked), application-level PUT pre-check, import, fork, MCP instruction patch | `internal/infra/db/repos/applications.go:724`, `:922`, `:949`; `internal/api/v2/applications/handler.go:930`; `internal/api/v2/eliteacore/handler.go:3275,3513,4347`; `internal/api/v2/mcp/internal_applications_version.go:176` |
| Main | Start path: the size bound is tested before the root-source capture (whose own 1 MiB bound refused 8 MB pipelines with the generic sentinel); the freeze pre-scan uses the shared constants and typed errors; the route answers the typed 422 | `internal/application/agentexecution/start.go:263`, `:726`; `internal/application/httpaction/frozen.go:141,161`; `internal/api/v2/agentexecution/route.go:641` |
| Web | Pure, idempotent, copy-on-write normalizer (safe integers only), applied at each load entry point; saved YAML text kept unless the user edits; Flow→Yaml re-dump that only re-spells ids is not stored | `src/shared/lib/pipelineNodeIdentifiers.ts:111`; `src/features/pipelines/lib/flow-editor/helpers/parsePipeline.helpers.ts:49`; `src/features/pipelines/model/pipelineYamlStore.ts:138`; `src/features/pipelines/lib/pipelineYamlDocument.helpers.ts:94`; `livePipelineGraphAdmission.ts`, `graphAdmission.helpers.ts` |
| Web | Non-string route targets reported by existing admission instead of dropped; Mermaid `sanitizeId` no longer throws (`id?.replace is not a function`) | `src/features/pipelines/lib/graphAdmission.nodeReads.ts:115`; `src/features/agents/lib/helpers/parseYamlToMermaid.helpers.ts:116` |
| Web | Save banner shows the server's readable refusal, falling back to "Failed to save your changes." | `src/pages/pipelines/ui/EditPipelineAlerts.tsx:65` |

No file changed by #1084 (`feat/rust-graph-point5-consolidation`) is touched. No new `t()` keys (the existing
`pages.pipelines.editPipeline.saveError` default is reused), so no `en.json` backfill is needed. No dependency, CI or
lockfile change.

### Worker and Web spellings

| YAML | Worker (serde_yaml_ng) | Web (js-yaml 5.4.2) |
|---|---|---|
| `1`, `+1`, `-1`, `-0` | `"1"`, `"1"`, `"-1"`, `"0"` | same |
| `0x1A`, `0o17` | `"26"`, `"15"` | same |
| `9007199254740991` / `9007199254740992` | admitted / refused (typed) | admitted / reported by admission |
| `1_000` | string `1_000` | same |
| `010` | string `010` | `"10"` (**differs**) |
| `0b11` | `"3"` | string `0b11` (**differs**) |
| `1.0`, `1e3` | refused as floats | `"1"`, `"1000"` (**differs**) |

js-yaml discards the scalar spelling, so the four exotic spellings cannot be aligned without a different parser;
recorded as follow-up 2.

## Tests

| Component | Run | Result |
|---|---|---|
| Worker | `cargo test --locked` | 1,946 passed, 0 failed, 2 ignored (pre-existing). PostgreSQL-only tests skipped: `ELITEA_TEST_DATABASE_URL` unset; this change has no PostgreSQL surface. |
| Worker | `cargo fmt --all -- --check`, `cargo clippy --locked --all-targets --all-features -- -D warnings` | clean |
| Main | `go test -race` on `domain/pipelinelimits`, `application/httpaction`, `application/agentexecution`, `api/v2/agentexecution`, `api/v2/applications`, `api/v2/eliteacore`, `api/v2/mcp`, `infra/db/repos`, `infra/storage`, `api` against a local PostgreSQL 18 | 5,444 passed, 0 failed, 11 skipped (all in `infra/db/repos`, pre-existing env-gated fixtures), then re-run after the two review fixes: `applications` 192, `repos` 1,658 (11 skipped), `agentexecution` + `api/v2/agentexecution` pass |
| Main | `go vet` touched packages; `gofmt -l` touched files | clean |
| Web | vitest `features/pipelines`, `pages/pipelines`, `features/agents`, `shared/lib` | 430 files, 4,430 passed, 0 failed, 0 skipped, 1 expected-fail (pre-existing) |
| Web | `tsc --noEmit`; lint `--deny-warnings`; `scripts/i18n-backfill.mjs --check` | clean |

New tests and what they proved red first:
- **Worker** `compiler_identifier_tests.rs` (12): failed with `malformed_yaml` before the change. Covers every
  position, digest equality with the quoted twin, a routed run reaching node `3`, duplicate `1`/`"1"`, the safe-range
  boundary, the spelling table, 20 refused shapes, no value in Display/Debug, null regression, idempotence, counts-only
  log. `compiler_limit_tests.rs` (6): 512 KiB and +1, 128 and 129 nodes, per-node 64 KiB and +1, five families, entry
  counts. Wire: `pipeline_tests.rs` (2) and `native_agent_lifecycle.rs` taxonomy (1) follow a real compiler error to
  the agent-settings safe message with `retryable=false`. **Caveat:** the limit and wire tests were written after the
  implementation; the old behaviour was proven by three pre-existing assertions that broke and were updated
  (`compiler_tests::whole_pipeline_yaml_is_bounded_strict_and_digest_stable`,
  `assembly_tests::a_pipeline_profile_admits_yaml_up_to_the_compiler_bound`,
  `assembly_tests::large_instructions_survive_agent_assembly_and_variable_rendering`).
- **Main** `pipelinelimits/limits_test.go` (6, red against a stub); PostgreSQL handler tests for create, create
  version, update version, agents unaffected, application PUT, existing over-bound version readable and editable,
  unknown version 404, and `TestHandlerPostgres_PipelineLimitsOnAgentTypeChange` (red: 201 and a stored 512 KiB+1
  pipeline); import/fork (6 each); MCP patch; `router_pipeline_limit_authz_test.go` (18 cases, foreign project /
  withheld / no permission → 403, never a size verdict); start: `TestCurrentPipelineStartRefusesTheSharedBoundsBeforeSourceCapture`
  (red: the over-bound pipeline reached the capture), `TestCurrentPipelineStartOverTheSharedBoundsReturnsTheTypedLimitError`,
  `TestCurrentApplicationStartRouteNamesThePipelineLimitItRefused`, `TestHTTPFreezeRefusalsNameTheSharedPipelineLimit`;
  numeric `transition: 2` added to the existing snapshot tests.
- **Web** `pipelineNumericNodeIds.integration.test.tsx` (25; 19 failed before with `id.replace` / `id?.replace is not
  a function`, numeric ids from `parseYaml`, false admission issues), `pipelineNodeIdentifiers.test.ts` (7), and a
  readable-refusal case in `EditPipeline.test.tsx` (red). The real `EditorPanel` renders nodes `1` and `2`; view
  switches leave `yamlCode` byte-identical and never call `setYamlDirty(true)`; after an edit the dump reparses with
  edges intact.

## Performance

**Budget.** No added I/O or round trip on any path; at most one bounded parse per save; start-path cost for an
in-bound pipeline below 2 ms at the 512 KiB bound.

| Mechanism | Measured |
|---|---|
| Worker normalization | One walk over the already-parsed `serde_yaml_ng::Value`; no extra parse, no allocation unless a value is rewritten. |
| Main save check `pipelinelimits.Check` | Byte length first (O(1)); one first-document YAML parse only for in-bound pipeline text; agents never parsed. The update path reads the stored `agent_type` only when the text is over a bound and the body names no type. The type-change path reads only `octet_length` for over-size stored text. |
| Main start check `currentPipelineStartBound` | One JSON decode of version details with `instructions` kept raw, unquoted only when over the bound. `go test -bench`: agent 10 KiB 25 µs / 11 KB / 8 allocs; pipeline 512 KiB 1.8–2.0 ms / 0.5 MB / 8 allocs. A first version that ran the full `Check` (second YAML parse) measured 13.6 ms / 4.6 MB at 512 KiB and was rejected; the node count is still named by the existing freeze pre-scan. |
| Web | Normalizer returns the same reference when nothing changes (no re-render churn); `isIdentifierRespellingRedump` parses the stored text once per `setYamlCode`, which is called on view switches and attachment sync, not per keystroke. |

## Durability

**Threats.** Changing persisted bytes, digests or checkpoint lineage; partial writes on refusal.

- No migration, state, checkpoint or recovery-path change.
- Saved YAML text is never rewritten by load or view switches (browser: version 179 text identical after Flow→Yaml→
  Flow→Yaml; version 186 stored exactly as typed).
- `definition_digest` is unchanged for every document that compiled before. Numeric-id pipelines were refused before
  (`MalformedYaml`), so none has checkpoints; their first lineage equals the quoted twin's digest. Normalization is
  deterministic and idempotent, so a replacement Worker computes the same digest on resume or takeover.
- A refused save writes nothing: create rolls back the transaction; the application-level PUT refuses before any
  write; import/fork refuse before the application row; the MCP patch refuses before cloning a backup (tests assert
  row counts; browser: application 126 description and 8,000,358-byte instructions unchanged after the refused save).

### Recovery guarantee rows

| Component × phase | Class | Enforcing code | Proof |
|---|---|---|---|
| Main × admission (save) | **F** — typed 400 naming the limit; nothing stored | `pipelinelimits/limits.go:51`, `repos/applications.go:724,922,949`, `eliteacore/handler.go:3275` | PostgreSQL handler tests; browser case C |
| Main × admission (start) | **F** — typed 422 naming the limit (was a misleading generic F); nothing admitted | `agentexecution/start.go:263,726`, `route.go:641` | `TestCurrentPipelineStartRefusesTheSharedBoundsBeforeSourceCapture`; browser case D |
| Worker × admission (compile) | **F** for limit and identifier refusals; unchanged **R** for admitted pipelines (numeric ids now admitted with a stable digest) | `compiler.rs:654,1655`, `runtime.rs:168`, `native_agent_lifecycle.rs:1459` | Worker identifier/limit/wire tests; browser cases A, B, D2 |
| Web/browser × editor load | No durable state; reload re-derives the same view from unchanged text | `pipelineNodeIdentifiers.ts:111`, `pipelineYamlStore.ts:138` | Web integration tests; browser reloads |
| Sandbox supervisor, NATS, PostgreSQL, LLM gateway | Not touched: no Code node, bus, schema or model path changed | — | — |

No **L** row is introduced.

## Resilience

- Everything bounded: 512 KiB / 128 nodes on save and start (one constant, `pipelinelimits`, mirrored by the Worker's
  `MAX_PIPELINE_YAML_BYTES`/`MAX_PIPELINE_NODES`); integer ids within ±(2^53−1); per-node caps unchanged.
- Typed, readable failures instead of generic text for size, node count and per-node bounds; identifier refusals are
  typed in the Worker (wire text still generic, follow-up 1).
- No silent coercion: only safe integers are rewritten; every other non-string value is refused (Worker) or reported
  by admission (Web), never stringified.
- No panics on request paths; normalization returns `Result`.

## Security

- **Authorization.** Unchanged and first: every new check runs behind the router's `projectPermission` gate (18-case
  authz test: a foreign or unpermitted caller gets 403, never a size verdict). New SQL reads are scoped to the caller's
  tenant schema (`tenantSchema` → `tenantschema.Quote`) with numeric-validated ids as bind parameters.
- **No content in errors, logs or events.** Refusal texts are fixed `apierr.APIError` values; Worker causes are
  `&'static str`; the lifecycle log adds static `cause_code`/`cause_detail`; the normalization log carries counts only.
  Tests embed sentinel content and assert it is absent.
- **Identifier integrity.** A decimal string cannot equal a reserved node name (`END`, `__elitea_subgraph_*`, Printer
  reset ids); duplicate normalized ids are refused; the strict validators still run after normalization. Main's HTTP
  grammar still requires a `!!str` id on `http` nodes, so numeric normalization cannot bypass it.
- **Web.** The server message is rendered as React text (no `dangerouslySetInnerHTML`).
- **Security review (2026-10-08, `security-review`).** No vulnerability meets the >80% confidence bar (SQL, tenant
  scoping, existence oracle, content echo, partial rows, identifier collisions, HTTP-grammar bypass, YAML parsing all
  checked).
- **Code review (2026-10-08, `code-review` high).** 8 findings: 1 fixed (agent-type change bypassed the save bound,
  `b8587bee`); the browser run then found and fixed the start-ordering gap (`f0b5b00b`); the rest are follow-ups below.

**Dependency audits.** No dependency changed; every finding is pre-existing on `main`.
- `govulncheck` v1.1.4 (Go 1.26.5): 7 reachable standard-library vulnerabilities fixed in Go 1.26.6 (GO-2026-6218,
  -6091, -6090, -6089, -6088, -5972, -5026); 1 imported-package and 5 required-module findings not called.
- `cargo deny --all-features check advisories`: RUSTSEC-2026-0258 (`h2`), RUSTSEC-2023-0071 (`rsa` via `sqlx-mysql`),
  yanked `chacha20 0.10.1`.
- `npm audit`: 7 findings (2 low, 1 moderate, 4 high); `package.json` and lockfile unchanged.
- No CI change (user decision).

## Browser evidence

**Stack.** A separate, isolated rehearsal stack was built because two other sessions were browser-testing the shared
candidate stack (`localhost:18094`), which was not touched:
- browser `http://p5ids.localhost:18095` (distinct host so the `elitea_session` cookie cannot collide), OIDC emulator
  `http://oidc-p5ids.localhost:19495`, signed in as `admin@centry.user`;
- private networks/volumes `*-p5ids20261008`; physical clone (`pg_basebackup`) of the candidate PostgreSQL cluster and
  a byte-identical copy of its RustFS data; fresh NATS JetStream assets via the repo bootstrap; every non-replaced
  service on the candidate's image IDs;
- images = the candidate's source revisions + the #1140 hotfix + this branch's commits, cherry-picked without textual
  conflict:
  - Main `elitea-main:current-nats-efa7213e803d-hotfix1140-p5ids-20261008-r2` `sha256:35e69d8c3455…ad99ce`
    (efa7213e + 024e6881 + ff8c757e + b8587bee + f0b5b00b; first run used `…-20261008` `sha256:98bee257…` without
    f0b5b00b);
  - Worker `elitea-worker-rust:frozen-cargo-c53ab7d5496a-hotfix1140-p5ids-20261008` `sha256:86a2daf88b8a…9efe`
    (c53ab7d5 + f1334945 + a2f23306);
  - Web `elitea-web:reload-42a0c390bc19-p5ids-20261008` `sha256:fa03562f223b…a393` (42a0c390 + ba7bc05b).
  On the c53ab7d5 lineage the Worker test target needed three test-only `pub(super)` visibility keywords (from #1137,
  already on `main`); the release binary is unaffected.

| Case | Pipeline (version) | Chat / execution | Result |
|---|---|---|---|
| A. Existing numeric-id pipeline (`entry_point: 1`, `id: 1`) | 153 (179) | 867 / `c4469e84…` | Editor renders node `1` (no error boundary, no `replace` error); Flow→Yaml→Flow→Yaml leaves the text byte-identical and does not mark it dirty; run answers `numeric-id-ok` (on #1140 the Worker refused it); one identical answer after reload. Worker log `numeric_identifier_count=2`. |
| B. New pipeline created in the editor, numeric `entry_point`, `id` and `transition` | 160 (186) | 868 / `fd65d118…` | YAML typed into the editor and saved; stored text equals the typed text; flow nodes `1`, `2`, `END`, edges `1→2`, `2→END`; run answers `numeric-chain-ok`; same after reload. Worker log `numeric_identifier_count=4`. |
| C. Save of an existing 8,000,358-byte version | 126 (133) | — | Version still loads; a one-character description edit + Save → `PUT …/version/prompt_lib/2/126/133` 400, banner "The pipeline definition exceeds the 512 KiB size limit. Reduce the YAML before saving."; database unchanged; loads again after reload. |
| D. Run of the 8 MB pipeline | 126 (133) | 869 (Main r1), 870 (Main r2) | r1: still "This agent turn requires the current execution path." — root-source capture refused first (fixed in `f0b5b00b`). r2: 422 `PIPELINE_INSTRUCTIONS_TOO_LARGE`, chat shows "The pipeline definition exceeds the 512 KiB size limit. …". A refused start admits nothing, so the chat is empty after reload (unchanged behaviour for start refusals). |
| D2. Run of a 480 KiB pipeline whose single LLM node exceeds its 64 KiB cap | 72 (79) | 871 / `c4e767bb…` | Chat shows "The request cannot start because the agent instructions or settings exceed a platform input limit. …" (was "The execution input is invalid." on #1140); same after reload. Worker log `error_code="native_agent.input_limit" cause_code="graph.pipeline.node_limit_exceeded" cause_detail="nodes[].llm"`. |

**Fixtures.** Pipeline 160 was created and saved entirely through the editor (YAML typed in the browser). Pipelines
153, 126 and 72 are existing records from the copied candidate database (153/154 were created through the editor by
#1140; 126's and 72's large YAML were written to the database earlier because they are too large to type). Every run,
save and reload went through the browser; no response was mocked.

## Open limits and follow-ups

1. **Catalog wiring for `graph.pipeline.invalid_identifier`.** Id-shape refusals still show "The execution input is
   invalid."; the cause is in the Worker log only. Needs a `RuntimeFailureKind` row in `src/protocol/output.rs`
   (`runtime_error_policy`, `canonical_runtime_failure`), its mapping in `assembly_failure`, and the allow-list entry in
   Main `internal/transport/runtimegrpc/output/server.go` `runtimeFailurePolicyForError`. Both files are owned by #1084;
   do it after #1084 merges.
2. **Exotic spellings differ between editor and Worker** (`010`, `0b11`, `1.0`, `1e3`; table above). The editor would
   need a spelling-preserving YAML reader for identifier scalars.
3. **Saved-child path** (`internal/infra/storage/runtime_saved_child_scope.go:70`) still maps the typed limit to
   `scope.ErrDenied` → generic runtime-context failure; needs a contract change for a readable child refusal. The MCP
   agent execute route (`internal/api/v2/mcp/execute.go`) keeps its generic "could not be started".
4. **Code and Map node size caps** still read as generic `Invalid`; the 256 state-key count stays `ResourceExhausted`.
5. **Web store bandaid.** `setYamlCode` ignores a pure id re-spelling re-dump because `EditorPanel.onSelectChatMode`
   re-dumps on every Flow→Yaml switch, and `useIsPipelineYamlCodeDirty` compares raw `load` output; both files are
   owned by #1084. Afterwards: skip the re-dump when unchanged and compare normalized documents (also fixes "undo
   after an edit stays dirty").
6. **Main duplicate check.** The application-level PUT pre-checks because it swallows `UpdateVersion` errors; the root
   fix is to stop swallowing them.
7. **Request-body cap.** These write routes decode the whole JSON body before the bound (pre-existing); a gateway or
   handler body limit is a separate hardening item.
8. **Wording.** The start refusal reuses the save text ("…before saving"); a run-specific sentence needs a product
   decision.
9. **Per-node 64 KiB caps vs the 512 KiB bound** — revisit later (user decision; values unchanged).
