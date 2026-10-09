# Pipeline YAML round-trip contract in the editor

Status: `complete` for the Web defect. No Main, Worker or contract change.

`P` is `apps/elitea-web/src/features/pipelines`.

## Defect and business behaviour

On merged main `1ab920dde`, opening pipeline 165 (version 191) on the real-model stack `elitea-verify-0def77b2` and
switching to the Yaml tab showed `Error dumping YAML: Pipeline YAML serialization changed its contract`. The editor then
held an empty graph. Graph admission said "nodes: a pipeline must hold between 1 and 128 nodes — this one holds 0", so
Save and attaching tools were blocked. The stored document (546 bytes, two direct `toolkit` nodes on the artifact
toolkit `test`) was valid. Pipelines 159 and 162 opened fine.

Behaviour kept from the current platform (EliteaUI `flowEditor.helpers.js:292-308`, `parsePipeline.helpers.js:654-664`):

- Opening a toolkit node whose `input_mapping` is empty fills it with the tool's schema defaults (required fields, and
  optional fields that have a non-empty default). This marks the document as edited, as it does in EliteaUI.
- A mapping entry carries `enum` when the tool schema declares one.
- A legacy inline `decision:` is migrated into a standalone Decision node.

Not ported: EliteaUI's js-yaml v3 dropped `undefined` members without a word. Here the serializer stays strict (it
refuses a document whose YAML would not load back as the same value), and the editor no longer stores its error text as
the document.

## Cause

Three suspects were named. Each was tested against the stored document:

| Suspect | Finding |
|---|---|
| Empty `input_mapping: {}` | Round-trips unchanged (`P/lib/dumpYaml.helpers.test.ts:179`). It only triggered the cause below. |
| `dict` state types | Round-trip unchanged; the state declaration order is kept (`P/lib/dumpYaml.helpers.test.ts:179`). |
| The editor filling toolkit nodes from the attached toolkit `test` | **The cause.** |

The load path for node `ls`:

1. `useFunctionInputMapping` (`P/lib/flow-editor/hooks/useFunctionInputMapping.ts:277-279`) sees an empty mapping and
   writes the `list_files` defaults: `bucket_name`, `folder`, `include`, `skip` (default `null`) and `recursive`
   (`false`).
2. `buildMappingEntry` built every entry as `{ type, value, enum: enumValues }`. None of these schemas has an enum, so
   each entry carried `enum: undefined`.
3. `EditorPanel` passed that document to `dumpYaml`. js-yaml drops `undefined`, the strict check compares the reloaded
   value, and the check refused the document. `dumpYaml` returned `Error dumping YAML: …`, and `EditorPanel` stored that
   string as `yamlCode`.
4. Graph admission parses `yamlCode` (`P/lib/livePipelineGraphAdmission.ts:89-96`). It read a plain string, that is a
   graph of 0 nodes, and blocked Save.

Pipelines 159 and 162 use MCP nodes with complete mappings, so no defaults were written for them.

A second producer of the same kind was found while testing. The legacy decision migration wrote `input: undefined` when
the legacy node had no `decisional_inputs`. The existing test "arms the dirty flag on a plain load of a pipeline holding
a legacy decision node" only passed because the error text was stored. It now passes because the migrated document
really serializes.

## Change

| Path | Change |
|---|---|
| `P/lib/flow-editor/helpers/flowEditorInputMapping.helpers.ts:222-226` | A mapping entry gets `enum` only when the schema declares one. A stale saved `enum` is dropped, which is the same result EliteaUI's dump produced. |
| `P/lib/flow-editor/helpers/parsePipeline.helpers.ts:123` | The migrated Decision node gets `input` only when there were legacy inputs. |
| `P/lib/dumpYaml.helpers.ts:132,160-167` | A refusal names the first member YAML does not carry back (`…: nodes[1].input_mapping.skip.enum has no YAML form`). |
| `P/lib/dumpYaml.helpers.ts:177` | `trySerializePipelineYaml` returns `{ yaml }` or `{ error }`. `dumpYaml` (`:186`) remains for comparisons only, and its comment forbids storing its result. |
| `P/lib/hooks/usePipelineYamlSerialization.ts:26-38` | The editor's strict write path. A refusal is kept together with the document the editor kept. It lapses once that document is replaced by typing, Cancel, a version switch or a successful write. |
| `P/ui/EditorPanel.tsx:231-237,309,326,365` | A flow write, a switch to Yaml mode and the legacy-decision effect all serialize strictly. A refused document is never stored: the stored text and graph stay, and an inline alert gives the reason. |
| `P/ui/PipelineYamlSerializationAlert.tsx` | An inline MUI `Alert`, as in `GraphAdmissionGate`. No modal. |
| `apps/elitea-web/src/shared/i18n/en.json` | `features.pipelines.editorPanel.serializationError`, written by `scripts/i18n-backfill.mjs`. |

## Tests

`apps/elitea-web`, Node 24.19 (`npx vitest run --config vitest.config.ts --project node`):

| Test | What it proves |
|---|---|
| `P/lib/flow-editor/hooks/useFunctionInputMapping.yamlContract.test.tsx:92` | The editor's load path for the stored document (fixture `P/__tests__/pipeline165Fixture.ts`; artifact schemas as Main serves them, served through MSW). It shows what the editor builds for `ls`, and that every document it hands on loads back as the same value from `serializePipelineYaml`. |
| `…yamlContract.test.tsx:110` | `mk` already maps its required `filename`, so nothing is written for it. The test waits for the schema instead of a fixed delay. |
| `P/lib/dumpYaml.helpers.test.ts:174,179` | The stored text is kept byte for byte when nothing changed. `input_mapping: {}`, the `dict` types and the state order survive a real edit. |
| `P/lib/dumpYaml.helpers.test.ts:187,193,199` | The refusal names the member, including a `Date`, which the default schema loads back as a string. The refusal comes back as a value. |
| `P/lib/flow-editor/helpers/flowEditorInputMapping.helpers.test.ts:40,67` | No `undefined`-valued key is written. `enum` appears only when declared, and a stale one is dropped. |
| `P/lib/flow-editor/helpers/parsePipeline.helpers.test.ts:68` | The legacy decision migration serializes. |
| `P/ui/EditorPanel.test.tsx:140,162,182` | An unserializable flow document keeps the stored text (still 2 nodes) and shows the readable alert, with no "Error dumping YAML". The alert lapses on Cancel. A round-tripping document shows no alert. |

Written first, these failed on `1ab920dde`: the 7 new or tightened assertions failed against the original four source
files and pass with the fix.

| Run | Result |
|---|---|
| `src/features/pipelines` | 227 files, 2523 passed, 1 expected-fail (pre-existing `it.fails`, `P/ui/settings/InputMappings/DataTypeValueField.test.tsx:146`), 0 skipped |
| Full `--project node` (final commit `d990a6f44`, run alone) | 1665 files, 16826 passed, 0 failed, 6 expected-fail (pre-existing), 1 skipped (pre-existing) |
| `tsc --noEmit`, `oxlint --deny-warnings` | Clean |
| `check-budgets`, `check-layer-cycle`, `check-partition`, `check-testid-namespace`, `check-stale-disclosure`, `check-visual-coverage`, `check-gates-selftest`, `i18n-backfill --check` | OK |
| `check-dead-code` | Fails on the pre-existing `FORBIDDEN_MARKDOWN_HTML_ATTRS` (`src/shared/ui/lib/sanitizeMarkdownHtml.ts:49`), which is also on `main`. Nothing from this change. |

## Performance

- Mechanism: one strict serialization per editor write, as before. The locator runs only on a refusal and walks the two
  values once (`P/lib/dumpYaml.helpers.ts:132-145`). Load writes stay at one `setYamlJsonObject` per node.
- Proving test: `P/lib/flow-editor/hooks/useFunctionInputMapping.yamlContract.test.tsx:110` (no write for a complete
  mapping).
- Measured: in the browser, opening the pipeline made no extra requests. Save made one version `PUT`.

## Durability

- Mechanism: the stored document is never replaced by text that is not YAML. A refused write leaves `yamlCode` and
  `yamlJsonObject` untouched (`P/ui/EditorPanel.tsx:236-237,309`). Save sends `yamlCode` as before.
- Proving test: `P/ui/EditorPanel.test.tsx:140`.
- Measured: the browser save stored 833 bytes, which loads to the same 3-node graph after a reload (below).

## Resilience

- Mechanism: a typed refusal value (`P/lib/dumpYaml.helpers.ts:177`) and a readable reason that names the member
  (`:166`). The editor never falls back to an empty graph, and the refusal lapses with the document it describes
  (`P/lib/hooks/usePipelineYamlSerialization.ts:36`). The strict check is kept, so nothing is coerced silently.
- Proving tests: `P/lib/dumpYaml.helpers.test.ts:187,199`; `P/ui/EditorPanel.test.tsx:140,162`.
- Measured: in the browser, the unfixed image showed the error text and 0 nodes. The fixed image showed the YAML, 3
  nodes and Save enabled.

## Security

- Applies: injection and construction (rendered error text) and secrets (diff scan). The alert renders the reason as a
  React text node, with no HTML sink (`P/ui/PipelineYamlSerializationAlert.tsx`). The reason is a key path from the
  editing user's own document, kept in local state only, and is never logged or sent. Authorization, identity, egress
  and parsers are unchanged: the client uses the existing js-yaml `load`, and Main and the Worker still admit on save
  and run.
- Proving test: `P/ui/EditorPanel.test.tsx:140` asserts the exact rendered text.
- Measured: `security-review` found nothing at confidence 8 or above. The secret-pattern scan of each commit was clean.
  `npm audit` shows the same 7 pre-existing findings (2 in production dependencies: katex, mermaid). There is no
  dependency change.

## Recovery guarantees

| Component × phase | Class | Note |
|---|---|---|
| Web/browser × editing before Save | L (pre-existing, by design) | Unsaved editor state is browser state and is lost on reload, as before. Not new. Code `P/ui/EditorPanel.tsx:233-241`. |
| Web/browser × a refused document | F | A typed, readable refusal. The stored text and graph are kept, so no partial result is lost. Code `P/lib/hooks/usePipelineYamlSerialization.ts:26-38`; test `P/ui/EditorPanel.test.tsx:140`. |
| Web/browser × Save | I | One version `PUT` of `yamlCode`. Re-sending the same text is idempotent. Unchanged by this PR. |
| Main, Worker, sandbox supervisor, NATS, PostgreSQL, LLM gateway | n/a | Not touched. |

## Real-browser evidence

- Stack: my own standalone stack `elitea-yamlrt`, at `http://yamlrt.localhost:18320/app/`. It has its own host, so its
  session cookies stay separate from the reference stack. It was restored from `product-real-models-main-1ab920dde.dump`
  without seeding. Migration ledger: shared 157, tenants 148.
- Images: everything is merged-main `main-1ab920dde-verify` (scheduler, inventory, engine and mocks at
  `main-0def77b22-verify`, as in the runbook), except `elitea-web`:
  - before: `elitea-web:main-1ab920dde-verify`;
  - after: `elitea-web:yamlrt-d5708ebb8`, then `yamlrt-d990a6f44` (image `sha256:fc8ffdb7…`, built from this branch).

  Image check (`docker create` + `docker cp`): the fix strings `has no YAML form` and
  `could not be written as pipeline YAML` are in the branch images (`usePipelineEditorLifecycle-*.js`) and absent from
  the main image. The page loaded `index-BKD17wt1.js`, which matches the final image.
- Steps, project 2, signed in through the stack's OIDC mock as the runbook's test admin:
  1. **Before** (main web image), pipeline 164, a copy of 165 v191: the admission banner read "this one holds 0" and
     the Yaml tab showed `Error dumping YAML: Pipeline YAML serialization changed its contract`. This reproduces the
     defect.
  2. **After** (fix image), same pipeline and data: 3 canvas nodes (`mk`, `ls`, `END`). The Yaml tab showed the
     document, there was no error text, no admission banner, and Save was enabled.
  3. Save. The DB now holds 833 bytes, the `ls` defaults as on EliteaUI. After a reload: 3 nodes, no unsaved state, no
     alert.
  4. The final image at `/app/pipelines/latest/165` (an untouched copy that happens to get id 165 / version 191, md5
     equal to the reference): 3 nodes, the Yaml tab shows the document, and after a full reload there is still no
     refusal. Pipelines 162 and 164 open clean.
- Not shown in the browser: the refusal alert itself. Both known producers are fixed, so no real document reaches it
  any more. It is proven by `P/ui/EditorPanel.test.tsx:140,162`.

## Fixtures

Created through the API on my own stack, not the UI. Browser sessions on Main `1ab920dde` drop about 30 s after
sign-in, and creating the pipeline was not the subject under test.

- `POST /api/v2/elitea_core/applications/prompt_lib/2`, with the exact stored text of reference version 191 (md5
  `d69050dd…`, 546 bytes).
- `PATCH /api/v2/elitea_core/tool/prompt_lib/2/5` with `has_relation: true`, which attaches toolkit `test` (artifact,
  id 5, from the dump).

Two copies were made: 164, which was later saved through the UI, and 165, which was never saved. The reference stack
was only read.

## Follow-ups

- Running these pipelines from chat fails in the Worker: `native_agent.invalid_input`, "a pipeline direct tool node
  references a tool outside its frozen scope". This happens for the untouched copy as well, and the reference stack
  logged the same refusal at 14:16:13Z, right after 165 was created. It is independent of this Web change and needs its
  own investigation.
- A refused flow write keeps the document, but `useOnNodeCreateAtPosition` still adds the canvas node
  (`P/ui/useFlowEditorNodeOperations.ts:92`). This is only reachable from a producer that writes `undefined`, and none
  is known after this PR. The fix needs a result from the flow-write contract.
- Opening a toolkit node with an empty mapping marks the pipeline edited (EliteaUI parity). This is a product decision.
- Confirm on the next rehearsal stack built from `main` with this change.
