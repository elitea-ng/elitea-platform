# Pipeline result correctness: list results, saved-child cards, application PUT, refused flow writes — 2026-10-09

Branch `fix/pipeline-result-correctness`. Follow-up package B of the 2026-10-09 triage:

| Item | Problem | Outcome |
|---|---|---|
| F4 | A pipeline's list result rendered as "Pipeline completed." in chat | Already fixed on `main` by #1161 at the root. Verified here on the root chat, the editor Test chat and through a saved child; the remaining saved-child display gap is F5. |
| F5 | The saved-child tool card always showed `{"response":"Pipeline completed."}` (`node_events.rs:339` on `main`) | The card carries the child's own result; the Web sub-agent preview shows that result, not the envelope. |
| F6 | Main's application `PUT` swallowed `UpdateVersion` errors (201 after a failed version write, rename kept) | The application and version writes share one transaction; a failed version write answers its own status and changes nothing. The handler's duplicate size check is gone. |
| F16 | A refused flow write kept the document but still added the canvas node | The flow-write contract reports a refusal; a node is added (and a deleted node removed) only when the document took the write. |

## 1. Business behaviour and sources

| Behaviour | Current platform (business reference) | This change |
|---|---|---|
| Saved child's tool result | The `Application` tool returns the child's final answer text to the parent (`elitea_sdk/runtime/tools/application.py` `_run` 828-861, `invoke` 264-298; text chosen by `extract_application_response_output` 65-146). | The card's `FunctionResponse` is `{"response": <the child's selected result>}`, the same text the parent receives (`node_events.rs:354`, `application.rs:844`). |
| Card display | EliteaUI shows a tool's output as a raw string, or pretty JSON for an object (`components/Chat/hooks.js` `AgentToolEnd` 1049-1090, `common/utils.jsx` `convertJsonToString` 845; modal `ToolModal.jsx` detects JSON). | The sub-agent preview unwraps the `{"response": …}` envelope (also the durable receipt shape with `values`) and shows a `json`-fenced result as the JSON itself (`subAgentActionOutput.ts`). Every other output keeps the baseline rendering. |
| List or object final answer | Fenced, pretty-printed JSON code block (`shared/lib/utils/jsonBlock.utils.js` `formatJsonBlock`; `Token.jsx` code blocks). | Unchanged from #1161: fenced pretty JSON (`pipeline_result.rs:264`). |
| Application `PUT` with a nested version | Application and version updated in one DB session, committed once; any version error answers 400 and nothing is committed (`elitea_core/api/v2/application.py` `put` 160-188). | One transaction (`applications.go:794-813`); any error answers its own typed status (400 limit, 400 for text PostgreSQL cannot store, 404, 500) and rolls back both (`handler.go:971-978`). |
| Adding a node | The legacy editor writes the YAML node and the canvas node together (`FlowEditor.jsx:205-261`); it has no strict serializer, so no refusal exists there. | The canvas node is added only when the document write was stored (`useFlowEditorNodeOperations.ts:81`). |

**Deliberately not ported:** the legacy card's raw `{output, messages, thread_id, …}` dict and Python `str()`
renderings; legacy's blanket 400 for unexpected failures (a database failure now answers 500 with a generic body).

## 2. Changed paths

| Component | Path | Change |
|---|---|---|
| Worker | `src/agents/graph/node_events.rs:339-358` | `send_application_end_scoped` takes the child's `response` and sends it as the card result. |
| Worker | `src/agents/graph/application.rs:844` | Passes the response the parent already validated (`finish_pipeline_output`). |
| Worker | `src/agents/graph/application_tests.rs:1090-1210, 1884-1906` | Real parent → saved child runs through `PipelineNodeEventStreamingAgent` and the ADK Runner. |
| Main | `internal/infra/db/repos/applications.go:419-440, 505-545, 785-817, 943-950, 956-995` | `getApplication`/`updateApplication`/`updateVersion` run on a `querier` (a version-only save reads inside the transaction); new `UpdateWithVersion` runs both writes in `WithinTx`; the pipeline bound checks take the same `querier`; SQLSTATE 22021 (text PostgreSQL cannot store) is a 400. |
| Main | `internal/domain/applications/repository.go:15-17` | `UpdateWithVersion` on the repository port. |
| Main | `internal/api/v2/applications/handler.go:950-996` | Calls `UpdateWithVersion`, writes its error; the duplicated `pipelinelimits.Check` pre-check is removed. |
| Web | `src/features/pipelines/lib/flow-editor/reactFlowTypes.ts:48-52` | `SetYamlJsonObject` returns `false` for a refused write (`void` from setters that never refuse still means stored). |
| Web | `src/features/pipelines/ui/EditorPanel.tsx:233-243, 317` | The flow write returns `false` on serializer refusal, `true` otherwise. `onAddNode` is collapsed to one line (file-length budget). |
| Web | `src/features/pipelines/ui/useFlowEditorNodeOperations.ts:41-49, 79-82, 148` | No canvas node and no viewport reveal after a refused write. |
| Web | `src/features/pipelines/ui/FlowEditor.tsx:150`, `lib/flow-editor/hooks/useIncompleteEdge.ts:17, 165-168` | Accept "nothing created"; the ghost-edge flow cleans up and connects nothing. |
| Web | `src/features/pipelines/lib/flow-editor/hooks/useDeleteItems.ts:111-116` | A refused delete write keeps the canvas nodes and edges (review finding: same divergence as F16). |
| Web | `src/features/chat-messages/lib/subAgentActionOutput.ts`, `ui/sub-agent-section/SubAgentAccordion.tsx:114-117, 131` | Sub-agent card preview shows the child's result; the preview is capped at 20rem and scrolls. |

`EditorPanel.tsx` is also touched by open PR #1084 (it replaces this callback with `editPipelineYamlDocument`). The
overlap is the callback's return value and one line of `onAddNode`; the user chose this over deferring F16.

## 3. Tests (counts and skips)

| Test | Proves | Red on the old code |
|---|---|---|
| `saved_child_card_shows_the_childs_result` (`application_tests.rs:1190`) | List child: card `{"response":"```json\n[…]\n```"}` equals the parent's answer; text child: card is the text. | `left: [{"response":"Pipeline completed.", …}]` |
| `a_large_saved_child_result_reaches_its_card_whole` (`application_tests.rs:1886`) | A 60 000-byte child result reaches the card event whole (above the 40 KiB event value). | n/a (bound proof) |
| `TestUpdate_VersionWriteErrorIsNotCreated` (`handler_test.go:316`) | 404 and 500 from the version write are answered, not 201. | `status = 201, want 404 / want 500` |
| `TestHandlerPostgres_ApplicationUpdateRollsBackWhenVersionWriteFails` (`pipeline_limits_postgres_integration_test.go:298`) | A version write only PostgreSQL refuses (NUL byte) answers 400 "…cannot be stored" and leaves the rename uncommitted. | `status = 201 … "name":"rb-renamed"` |
| `TestHandlerPostgres_PipelineLimitsOnApplicationUpdate` (`:268`, existing) | The over-bound refusal still changes nothing without the handler pre-check. | — |
| `useFlowEditorNodeOperations.test.ts:221-298` (4 tests) | Refused write: no node, no reveal; `true`/`undefined` still add; Condition is canvas-only. | `expected { id: 'Agent_1', … } to be undefined` |
| `useDeleteItems.test.ts:186` | Refused delete write: canvas keeps `A`, `B` and the edge; the dialog closes. | `expected [ 'B' ] to deeply equal [ 'A', 'B' ]` |
| `useIncompleteEdge.test.tsx:266` | Nothing created: no `onConnect`, dropdown reset, ghost cleaned. | `Cannot read properties of undefined (reading 'id')` |
| `EditorPanel.flowWrite.test.tsx:54, 69` | Real `EditorPanel` → lazy `FlowWrapper`/`FlowEditor` → real Add node menu: control adds `Agent_1`; with the `enum: undefined` fixture the canvas stays `['END','mk','ls']` and the stored YAML is untouched. | `expected [ 'END', 'mk', 'ls', 'Agent_1' ] to deeply equal [ 'END', 'mk', 'ls' ]` |
| `subAgentActionOutput.test.ts` (3), `SubAgentAccordion.test.tsx` (3) | Envelope unwrapped, fence dropped, other outputs unchanged; the component renders it; the preview height is capped (`320px`, `overflow: auto`). | `expected 'rp-list-child{"response":"```json…'` |

Suite runs:
- Worker: `cargo test --offline --locked --all-targets --all-features` against a disposable `pgvector/pgvector:0.8.1-pg18`
  with `ELITEA_REQUIRE_POSTGRES_RECEIPT_TESTS=1`, `ELITEA_TEST_DATABASE_GUARD=disposable-pg18`: 1 835 passed, 0 failed, 71 ignored across 12 test binaries (library: 1 740 passed, 71 ignored).
  `ELITEA_REQUIRE_NATS_SECURE_TEST` was not set locally (CI provisions it). `cargo clippy --all-targets --all-features
  -- -D warnings` and `cargo fmt --check` are clean.
- Main: `go test ./internal/api/...` (unit) ok; `go test ./internal/api/v2/applications/ -run Postgres` 48 passed, 0
  skipped; `go test ./internal/infra/db/repos/ -run 'Application|Version|Pipeline'` 94 passed, 0 failed, 0 skipped,
  both with a disposable `pgvector/pgvector:pg16` (`ELITEA_TEST_DATABASE_URL`). `go vet` and `gofmt -l` clean.
  `golangci-lint` is not installed locally.
- Web: `npx vitest run src/features/pipelines src/features/chat-messages` 283 files, 3 175 passed, 3 expected
  failures (pre-existing `it.fails`), 0 skipped; `npx tsc --noEmit` and `oxlint` on every changed file clean.

## 4. Performance

- **Worker** (`node_events.rs:354`): the card event carries the child result the parent already holds. It is the
  `String` already bounded by the child's result node (`MAX_PIPELINE_RESULT_BYTES`, 512 KiB, `pipeline_result.rs:21`),
  cloned once into the event. No new I/O, round trip or checkpoint write. A result above 40 KiB is chunked by the
  existing tool-result projection (`events.rs:2794-2860`, `MAX_TOOL_EVENT_VALUE_BYTES` `:92`), proven in the browser
  at 60 000 and 300 000 bytes (§7).
- **Main** (`applications.go:801`): the two statements that were two autocommit transactions are now one
  transaction: same statement count (2 writes, plus the size check's lookup only when over the bound), one
  `BEGIN/COMMIT` instead of two. The handler's duplicate `pipelinelimits.Check` parse is removed, so an in-bound
  pipeline save parses the YAML once instead of twice.
- **Web**: one boolean per flow write; the sub-agent preview parses an output string once per render
  (`JSON.parse`), bounded by what the card already holds.

## 5. Durability

- **F6**: durable intent and effect are one transaction (`tenant.NewExecutor(...).WithinTx`, `applications.go:801`). A
  failure or crash between the application write and the version write leaves neither (proven:
  `TestHandlerPostgres_ApplicationUpdateRollsBackWhenVersionWriteFails`; browser §7 before/after).
- **F5**: no checkpoint, channel or event format change. The card text comes from the child's checkpointed result, so a
  resumed child (Worker restart, P12) produces the same card. The card output is persisted by Main as before
  (`chat_message_trace_step.tool_output`, trace steps 8620, 8629, 8632 in §7).
- **F16**: a refused write stores nothing in either the document or the canvas, so a later Save cannot persist a node the
  YAML lacks.

## 6. Resilience

- No silent coercion: F6 removes a silent success (201 after a failed write); errors are typed `apierr` values
  (`handler.go:975`): text PostgreSQL cannot store is a 400 (`applications.go:947`), any other database failure a
  generic 500 without SQL text.
- F16 removes a silent divergence between canvas and document for node creation and deletion; the ghost-edge path now
  handles "nothing created" instead of throwing on `newNode.id`.
- F5 keeps the existing failure ordering: the card closes before the parent's output mapping is checked; an
  over-large mapped value still fails with the readable `pipeline.result_invalid` refusal, unchanged from `main`
  (§7, 300 KB case on both images).

## 7. Security

| Category (`rules/security.md`) | Applies | How checked |
|---|---|---|
| Trust boundaries and identity | No | No identity read or header trusted. |
| Authorization | Yes (F6 route) | No route or permission change: `PUT /application/prompt_lib/{projectID}/{applicationID}` keeps `models.applications.application.update` (`router.go`); the version-belongs-to-application check stays before the transaction; the transaction is tenant-scoped (`tenant.Project{ID: project}`). |
| Input, parsing, amplification | Yes | The pipeline bound is enforced in the repository inside the transaction (one parse instead of two). The Web preview parses an already-bounded string. |
| Injection | Yes | Parameterized SQL only; the refactor changes the receiver (`querier`), not the statements. |
| XSS | Yes (Web) | The preview renders text inside a `<pre>` as a React text node; no HTML or Markdown is introduced. |
| Egress | No | No outbound call. |
| Secrets | No | No secret; error bodies stay generic (`{"error":"internal server error"}`, or the fixed 400 text, which echoes no input). |
| Supply chain | No dependency change | `npm audit --omit=dev`: 2 low (pre-existing katex/mermaid); `govulncheck` on the changed Main packages: none; `cargo deny --offline --all-features check advisories`: RUSTSEC-2023-0071 (`rsa`, pre-existing, no fix) from the local advisory DB snapshot. No new findings. |

**Reviews.** `code-review` (high) on the full diff: 6 findings; 4 fixed in this PR (refused delete keeps the canvas,
capped preview, 400 for unstorable text, version-only save reads inside the transaction), 2 skipped with reasons
(the `{"response"[, "values"]}` unwrap is the application-tool contract; the older error-path test keeps its own
harness). `security-review` on the branch: no finding at or above the confidence bar; categories checked as in the
table above (SQL is parameterized and tenant-schema-qualified, the version write stays scoped by
`application_id AND id`, route permission unchanged, the 22021 refusal text is fixed, the preview is a React text node,
the card result is the text the parent already receives on the same stream).

## 8. Recovery guarantees

| Component × phase | Class | Enforcing code | Proof |
|---|---|---|---|
| Worker × P12 (nested pipeline child) | R/F¹, unchanged (`recovery-guarantees.md:280`) | Card text comes from the child's checkpointed result (`application.rs:788-844`). | `saved_child_card_shows_the_childs_result`; browser §9 |
| Worker × P14 (output delivery) | R (container) / F (pod), unchanged (`:282-283`) | Large card output goes through the existing chunked tool-result frames (`events.rs:2794`). | `a_large_saved_child_result_reaches_its_card_whole`; browser 60 000/300 000 bytes |
| Main × P14 | I/R, unchanged (`:246-249`) | Trace step persistence unchanged. | Trace steps 8620/8629/8632 |
| Main × application save (API, not an execution phase) | Atomic: all or nothing (was: partial commit) | `applications.go:801` | Postgres test above; browser §9 |
| Web/browser | No recovery-class change | — | — |

No L row is added.

## 9. Real-browser evidence

**Stack.** Own standalone stack `elitea-respipe` at `http://respipe.localhost:18160` (`STANDALONE_HOST=respipe.localhost`,
ports 18160-18167, OIDC 19560), real backend, NATS, PG18, real-model dump `product-real-models-main-c0f2e5f9b.dump`,
no response mocks. Signed in as `admin@centry.user` (Private project 2) through the local OIDC mock.

**Images.**
- Before: `main-c0f2e5f9b-verify` Worker `sha256:500d9904cc77`, Main `sha256:3a2ac118e53b`, Web `sha256:3f30846074d8`.
- After: Worker `elitea-worker-rust:respipe-f5` `sha256:a272a4e48b96`, Main `elitea-main:respipe-f6` `sha256:d079781d9efb`,
  Web `elitea-web:respipe-web` `sha256:3e50cfc77958`, built from this branch's tree (on top of #1160); after the code
  review fixes, Main `elitea-main:respipe-f6b` `sha256:fc59c39af1df` and Web `elitea-web:respipe-web2` `sha256:8dc189faeb24`
  (the Worker source did not change after its build; only tests and docs did).
- Binary identity: the Worker binary was extracted (`docker create` + `docker cp`); its SHA-256 (`93800a6ca82185cf…`)
  differs from the `main` image's (`8fc66e61e8c149ed…`). This change adds no new string or symbol, so identity is shown
  behaviourally: the same chat, the same child, `pipeline:delegate:0` card output `{"response":"Pipeline completed."}`
  on the `main` image and the child's list on this image (trace steps 8617 vs 8620; 8634 vs 8632 at 300 KB).

**Fixtures.** Created through the API with the signed-in cookie (`POST /api/v2/elitea_core/applications/prompt_lib/2`):
pipelines `rp-list-child` (166, a `state_modifier` writing a list of records), `rp-list-parent` (167, one `agent`
node `tool: rp-list-child`, `output: [reply]`) and `rp-scratch` (168). The child was attached to the parent **in the
UI** (editor Tools → Pipeline picker, Save; stored as `entity_tool_mapping` row 120). Chats 880 and 881 were opened from
the editor's Chat button.

| # | Check | Before (`main` images) | After (this branch) |
|---|---|---|---|
| 1 | F4: chat 880, `rp-list-child`, input `hello` | Fenced pretty JSON list | Same |
| 2 | F4: editor Test chat of 168, input `editor` | — | Fenced pretty JSON list (`qty: 6`) |
| 3 | F5: chat 881, parent → child, card `RP-LIST-CHILD` | `rp-list-child {"response":"Pipeline completed."}` | Preview shows the child's list as pretty JSON (`qty: 9`, then `qty: 5` for `final`); the parent's answer is the same list |
| 4 | F5: child result 60 000 bytes | — | Card preview 60 013 characters live; trace step 8629 stores 60 015 bytes |
| 5 | F5: child result 300 000 bytes | Card "Pipeline completed.", then the readable `pipeline.result_invalid` refusal | Card holds the whole 300 000-byte result (trace step 8632), then the same readable refusal (pre-existing parent mapping bound) |
| 6 | F6: `PUT` rename + version write PostgreSQL refuses (NUL) | **201**, application renamed `rp-list-child-RENAMED`, version unchanged | `respipe-f6`: **500** `{"error":"internal server error"}`; `respipe-f6b`: **400** `{"error":"the version contains a character that cannot be stored"}`; name and instructions unchanged; a following version-only `PUT` answers 201 |
| 7 | F6: `PUT` rename + 600 KiB instructions | — | **400** "The pipeline definition exceeds the 512 KiB size limit…", name unchanged |
| 8 | F6: editor rename `rp-scratch` → `rp-scratch-saved` + Save | — | `PUT … /168` 201; after reload the name and the added `Printer_1` persist |
| 9 | F16 regression: Add node → Printer on 168 | — | Canvas `['END','shape','Printer_1']`, YAML holds `Printer_1` |
| 10 | Delete regression: Node actions → Delete → Remove `Printer_1` on 168 (`respipe-web2`) | — | Canvas `['END','shape']`, YAML no longer holds `Printer_1` |
| 11 | Capped preview (`respipe-web2`), chat 881, input `web2` | — | Card preview is the child's list (`qty: 4`), computed `max-height: 320px`, `overflow: auto` |

- **Reload.** After a reload the sub-agent card shows the child name only on both images: the reload view never loads a
  sub-agent step's `tool_output` (lazy trace detail) and clicking the card fetches nothing. Main has the output
  (trace steps above). Pre-existing; recorded as a follow-up.
- **F16 refusal in the browser.** No UI producer of a non-serializable document is known after #1186, so the refusal
  path cannot be driven in a real browser; it is proven on the real `EditorPanel` render path in jsdom
  (`EditorPanel.flowWrite.test.tsx:69`).
- The stack was torn down after the evidence was recorded (`docker compose -p elitea-respipe down -v`).

## 10. Follow-ups

1. **Reloaded sub-agent card has no output.** `SubAgentAccordion` previews `toolOutputs`, which the reload path leaves
   empty (heavy columns are fetched one step at a time), and its click handler loads no detail. Legacy opens a modal
   with the fetched `tool_output`. Wiring lives in `convertMessagesToChatHistory.ts`/`ChatMessageList.tsx` (owned by
   #1084); do after it merges.
2. **`state_modifier` output cap.** A rendered template above 8 KiB (`libs/rust/agent-runtime/src/graph/state_modifier.rs:25`)
   fails as the generic "The runtime operation failed." (`pipeline.application_node.failed` when inside a child). This is
   the same class as F7 (limits mapped to generic failures).
3. **Durable-receipt card shape.** The receipt path (`application.rs:790-835`) closes the card with
   `{"response", "values"}`; production chat composition binds no call owner (`pipeline.rs:704-708`, call id format `application.rs:645`) and uses the
   plain path. The Web preview handles both shapes.
4. **Connect helpers.** `ConnectionOperationsHelpers` (`useConnectNodes.ts:55-77`) still update edges without reading the
   flow-write result; with no known producer of a refused document, they are left for a follow-up.
5. **#1084 rebase.** #1084 replaces `EditorPanel`'s flow write with `editPipelineYamlDocument`, which throws on refusal;
   on rebase it should return `false` instead of throwing so `useFlowEditorNodeOperations` keeps the canvas consistent.
