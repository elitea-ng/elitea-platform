# Pipeline UI Wave U3: Per-Node Panel Parity, Extension Node Card Summaries, Canvas-First Create, and List Polish (FU-283)

- **Date:** 2026-11-24
- **Branch:** `feat/fu283-u3-node-panel-polish` (based on `origin/main` `2df0a49cf`)
- **Reference Baseline:** `EliteaUI` `origin/main` + `GAP-MAP.md` (§1 rows 11–12; §2 per-node panel table; §5 extension node cards) + `pipeline-ui-u2-node-panels-20261123.md`

## 1. Business Behavior Ported & Replatform Contracts Preserved

| Area | Ported / Wired Behavior | Replatform Contract Preserved | Code Anchors |
|---|---|---|---|
| **Agent node (`AgentNode.tsx`)** | Integrates `AgentVersionSelect` inside Basic when an attached agent tool is bound; splits input mappings via `InputMappingFields` into Basic (`task` + required inputs) and Advanced (`Input`, `Output`, `chat_history` + optional mappings, `CommonInterruptSettings`, `NodeSettings`). | Preserves `attachedOnly` picker filtering, `AgentOrphanWarning`, Q6 `CommonInterruptSettings`, 5b-1 `NodeSettings`, and read-only/running `disabled` gating. | `apps/elitea-web/src/features/pipelines/ui/nodes/AgentNode.tsx`, `ui/settings/InputMappings/InputMappingFields.tsx` |
| **Toolkit / MCP nodes (`BaseToolNode.tsx` + `BaseToolNode.parts.tsx`)** | Adds `AddOptionalParameterButton` menu in Advanced to reveal optional tool inputs on demand without mutating YAML until edited; adds `McpDisconnectedHint` (`PickerMcpStatus`) under `ProjectToolkitPicker` when an offline MCP server is selected; splits required parameters (Basic) from optional parameters + `Input`/`Output`/`CommonInterruptSettings`/`NodeSettings` (Advanced); wires `nodeFieldInfo` tooltips onto `Tool` and parameter headings. | Preserves `ToolListError` retry/discovery-disabled handling, `#2432` atomic clear of `toolkit_name`/`tool`/`input_mapping`, legacy `FunctionNode` layout isolation, and full read-only `disabled` gating. | `apps/elitea-web/src/features/pipelines/ui/nodes/BaseToolNode.tsx`, `ui/nodes/BaseToolNode.parts.tsx`, `ui/settings/InputMappings/AddOptionalParameterButton.tsx` |
| **Core node panels (`LLM`, `Code`, `Decision`, `Router`, `HITL`, `StateModifier`, `Printer`)** | Aligns `NormalDecisionNode` label to `"Decision criteria"` with `nodeFieldInfo` tooltips on `Decision criteria` and `Decision outputs`; splits `LLMNode` `SimpleLLMInputs` into Basic (`system`, `task`) and Advanced (`chat_history`) with `nodeFieldInfo` tooltips; wires consistent `nodeFieldInfo` tooltips onto `CodeNode`, `RouterNode`, `HITLNode`, `StateModifierNode`, and `PrinterNode` (`SimpleLLMInputItem` `nodeType` prop + `InfoTooltip` beside `AIAssistantInput`/`RouteSelect`). | Preserves replatform extras (`LlmOutputContractSettings`, `CodeLanguageSelect`, `CodeDebugSettings`, Q6 `CommonInterruptSettings`, 5b-1 `NodeSettings`, `NodeErrorOutputHandle`). | `apps/elitea-web/src/features/pipelines/ui/nodes/{LLMNode,CodeNode,RouterNode,HITLNode,StateModifierNode,PrinterNode}.tsx`, `ui/nodes/DecisionNode/{NormalDecisionNode,DecisionNodeShared}.tsx`, `ui/settings/{SimpleLLMInputs,SimpleLLMInputItem}.tsx` |
| **Extension node cards & Basic/Advanced split (`Map`, `Parallel`, `SplitOut`, `Aggregate`)** | Refines 2-line `NodeCardView` summaries in `nodeCardV2.helpers.ts` (adding output/destination fallbacks when `join`/`destination` are omitted); removes obsolete 750px/650px inline-form heights in `NodeHeightMap` (now `160` for `Parallel`, `Map`, `SplitOut`, `Aggregate`); splits `MapSettings`, `ParallelSettings`, `SplitOutSettings`, and `AggregateSettings` into Basic and Advanced drawer sections via `ExtensionNodeShell` `renderAdvancedSettings` and `section` prop (default `'all'` for backward compatibility). | Preserves all YAML round-trip invariants, opaque metadata preservation, policy transitions, admission issue reporting, and `NodeSettings` inside Advanced. | `apps/elitea-web/src/features/pipelines/lib/flow-editor/constants/flowEditor.constants.ts`, `ui/node-card-v2/nodeCardV2.helpers.ts`, `ui/nodes/{MapNode,ParallelNode,SplitOutNode,AggregateNode}.tsx`, `ui/settings/graph-extensions/{ExtensionNodeShell,MapSettings,SplitOutSettings,AggregateSettings}.tsx`, `ui/settings/parallel/ParallelSettings.tsx` |
| **Canvas-first `CreatePipeline.tsx` (Gap #12)** | Mounts `ConfigurationTab` (left configuration form + live interactive `FlowEditor` canvas with `FlowEmptyState` when cleared + locked test pane) synced via `usePipelineVersionSync` and read on Save via `usePipelineGraphDraft`, with `resetPipelineDraft()` cleanup on Save/Cancel/unmount. | Strictly preserves `CreatePipelineAdmissionAlert`, live and click-time `useLivePipelineGraphAdmission` veto, `PIPELINE_STARTER_TEMPLATE` fallback, `#133` unsaved-changes guard, and server refusal messages. | `apps/elitea-web/src/pages/pipelines/CreatePipeline.tsx`, `pages/pipelines/lib/createPipeline.helpers.ts` |
| **Pipelines list polish (`PipelineListPanel.tsx`, Gap #11)** | Aligns empty-state description copy (`"Create your first pipeline to start automating tasks and workflows."`) and CTA label (`"Create pipeline"`) across `PipelineListPanel`, `PipelinesWorkshop`, and `PrivatePipelinesList` within existing backend APIs. | Does not invent unbacked server-side pagination, folder, or like endpoints. | `apps/elitea-web/src/pages/pipelines/ui/PipelineListPanel.tsx`, `pages/pipelines/{PipelinesWorkshop,PrivatePipelinesList}.tsx`, `shared/i18n/en.json` |

## 2. Deliberately Not Ported / Follow-Ups

- **Backend list pagination/like/trending endpoints:** Deliberately omitted; `PipelineListPanel` stays strictly within existing backend APIs.
- **Linux Playwright visual baseline PNG regeneration:** Not regenerated on macOS. Expected visual baseline shifts on CI for `/pipelines/create` (now renders the canvas-first workspace instead of a form-only page) and any pipeline canvas layout snapshots containing `Parallel`/`Map`/`SplitOut`/`Aggregate` nodes (due to compact `160px` heights in `NodeHeightMap`).
- **Real-browser live-stack verification:** Deferred (no Docker stacks started for this pure-Web UI slice; will be verified on Wave U3 integration stack).

## 3. Verification Summary

- **Typecheck (`npm run typecheck`):** PASS (`0` errors)
- **Lint (`npm run lint -- --deny-warnings`):** PASS (`0` warnings, `0` errors across 6,670 files)
- **Theme / Budgets / TestID / Layer / Dead Code / i18n:**
  - `npm run theme-gate` — PASS (all 9 gates + `brandPack.contract.test.ts` 5/5)
  - `node scripts/check-budgets.mjs` — PASS (7,479 files)
  - `node scripts/check-testid-namespace.mjs` — PASS (2,957 test ids)
  - `node scripts/check-layer-cycle.mjs` — PASS (5,984 modules, 0 errors/warnings)
  - `node scripts/check-dead-code.mjs` (Knip) — PASS (`0` issues)
  - `node scripts/i18n-backfill.mjs --check` — PASS (4 new keys added, 1 drifted key aligned in `src/shared/i18n/en.json`)
- **Dependency Audit (`npm audit`):** 21 pre-existing moderate dev-dependency advisories (`glob`/`minimatch` under `@storybook/react-vite` and `orval`); `0` new advisories and `0` dependency changes.
- **Vitest (`nice -n 10 npx vitest run --maxWorkers=2 src/features/pipelines/ src/pages/pipelines/`):**
  - **309/309 test files passed** (`3,303 passed | 1 expected fail (3,304)`, `0` unexpected failures; the 1 expected fail is the pre-existing `DataTypeValueField.test.tsx:146` `it.fails` for issue #1930).
  - Includes new/updated tests in `AgentNode.test.tsx`, `BaseToolNode.test.tsx`, `AddOptionalParameterButton.test.tsx`, `NodeCardV2.test.tsx`, `MapNode.test.tsx`, `ParallelNode.test.tsx`, `SplitOutNode.test.tsx`, `AggregateNode.test.tsx`, and `CreatePipeline.test.tsx`.
- **Secret Scan:** `grep` on `git diff origin/main..HEAD` — `0` hits.

## 4. Performance / Durability / Resilience / Security (`rules/security.md`)

- **Performance:** Pure React/MUI presentation and memoized selectors; replacing 750px/650px inline-form height estimates with `160px` eliminates oversized layout gaps for view-only extension cards.
- **Durability:** N/A (client-side UI only; preserves YAML document, `pipeline_settings`, and admission persistence contracts).
- **Resilience:** `CreatePipeline.tsx` enforces both live and click-time `useLivePipelineGraphAdmission` checks before submitting, cleans up Zustand draft state via `resetPipelineDraft()` on Save/Cancel/unmount, and preserves `ToolListError` retry states.
- **Recovery Guarantees:** Web/browser authoring only — no server/Worker execution path modified; existing admission and save-time durability guarantees remain unchanged.
- **Security Checklist (`rules/security.md`):**
  - **AuthN / AuthZ & Read-Only Gating:** Verified — `AgentVersionSelect`, `AddOptionalParameterButton`, `InputMappingFields`, extension Basic/Advanced sections, and `CreatePipeline` save controls all enforce `disabled` / `isRunningPipeline` / admission gates.
  - **Prototype Pollution / Dynamic Key Lookups:** Verified — `Object.hasOwn` guards on `InputMappingFields`, `useToolNodeParameterPartition`, and `EXTENSION_SUMMARY_EXTRACTORS`.
  - **XSS / Sanitization:** Verified — all user-authored labels, parameter names, and descriptions render through React text nodes; no `dangerouslySetInnerHTML`, `eval`, or raw HTML sinks introduced.
  - **Secrets / Supply Chain:** Verified — 0 secret scan hits; 0 new dependencies; `npm audit` reports 0 new advisories.
  - **Trust Boundaries / Egress / SQLi / Filesystem / Crypto / Sandbox:** N/A (pure browser React UI; no server endpoints, SQL, egress, sandbox, or crypto touched).
