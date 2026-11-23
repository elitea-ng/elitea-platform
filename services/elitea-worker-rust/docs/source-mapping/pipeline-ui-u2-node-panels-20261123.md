# Pipeline UI Wave U2 Part A: View-only Node Cards, 400px Node Config Drawer, Grouped Tool Pickers, `nodeFieldInfo`/`nodeDocs`, and `NodeSettings` Integration (FU-283)

- **Date:** 2026-11-23
- **Branch:** `feat/fu283-u2-node-panels` (based on `origin/main` `54718237f`)
- **Reference Baseline:** `EliteaUI` `origin/main` (`46d85cda7`) + `GAP-MAP.md` (§1 rows 1–2, 15, 17, 25; §2–§3) + `WAVE-U1-PLAN.md`

## 1. Business Behavior Ported & Replatform Contracts Preserved

| Area | Ported / Wired Behavior | Replatform Contract Preserved | Code Anchors |
|---|---|---|---|
| **Dual-mode `NodeCard` -> `NodeCardView`** | On the canvas (`!NodeConfigPanelContext`), `NodeCard` renders the compact view-only `NodeCardView` (280px width, header icon tile + display type + Entrypoint/Deprecated badges, 2-line read-only summary, `< 0.6` zoom mini-card via `@xyflow/react` `useStore`, and warning status icon/tooltip). Inside `NodeConfigPanelContext`, `NodeCard` renders the node's form children (`TriggerTypeSelector` for entrypoints, `NodeAdmissionIssues`, and Basic/Advanced sections). | Preserves `data-performing`, all normal `CustomHandle`s, `NodeErrorOutputHandle` (`error_output`), and admission issue reporting via `useNodeAdmissionIssues(id)` mapped directly into `NodeCardV2StatusIcon` tooltip. | `apps/elitea-web/src/features/pipelines/lib/flow-editor/flowEditorContext.ts:83-94`, `apps/elitea-web/src/features/pipelines/ui/nodes/BaseNode/NodeCard.tsx:1-125` |
| **400px `FlowNodeConfigDrawer` + Flow\|YAML + Name + Deprecation + Duplicate** | Mounts `FlowNodeConfigDrawer` in `FlowEditor.tsx` (strictly minimal diff) driven by single-node selection (`End` and `Ghost` excluded). Renders `NodeConfigPanelHeader` (type icon tile, description, safe `nodeDocs` link with `rel="noopener noreferrer"`, close), Flow\|YAML `TabGroupButton`, `NodeNameField` (compiler-legal + duplicate validation + `nodeFieldInfo` tooltip), `NodeDeprecationBanner`, per-node `NodeYamlView` (`useNodeYamlEditor` with single-node YAML validation & rename propagation), and `NodeConfigFooter` (Entrypoint / Make Entrypoint, disabled Q2 "Build with AI" with "Coming soon", Duplicate via `duplicatePipelineNode`, Delete). | Preserves compiler-legal node ID validation (`isCompilerLegalNodeId`), `renameYamlDocument`/`renameFlowNode`/`renameFlowEdge` reference rewriting (including `settings.error_transition`), read-only Flow view for refused/deprecated nodes, and clean edge-stripping on duplicate (`cloneYamlNodeForDuplicate`). | `apps/elitea-web/src/features/pipelines/ui/node-config-drawer/{FlowNodeConfigDrawer.tsx,NodeNameField.tsx,NodeDeprecationBanner.tsx,useNodeYamlEditor.ts,duplicateNode.helpers.ts}`, `apps/elitea-web/src/features/pipelines/ui/nodes/BaseNode/NodeCardHeader.rename.ts:31-67`, `apps/elitea-web/src/features/pipelines/ui/FlowEditor.tsx:54,259-275,346-353` |
| **Basic / Advanced Accordions, `nodeFieldInfo`, Q6 Interrupts, `NodeSettings`** | Splits admitted node forms into `NodeConfigSection section="basic"` (open) and `section="advanced"` (collapsed), wiring `NodeFieldHeading` (`nodeFieldInfo` tooltips), Q6 `CommonInterruptSettings`, and 5b-1 `NodeSettings` inside Advanced across LLM, Agent, Toolkit/MCP (`BaseToolNode`), Code, Router, Decision (`NormalDecisionNode`), HITL, Printer, State Modifier, Parallel, and `ExtensionNodeShell` (Map, SplitOut, Aggregate). | Preserves `LlmOutputContractSettings`, Code `Language`/`Debug`, `NodeSettings` 5b-1 family gating, `NodeErrorOutputHandle`, and `NodeAdmissionIssues`. | `apps/elitea-web/src/features/pipelines/ui/nodes/{LLMNode,AgentNode,BaseToolNode,CodeNode,RouterNode,HITLNode,PrinterNode,StateModifierNode,ParallelNode}.tsx`, `ui/nodes/DecisionNode/NormalDecisionNode.tsx`, `ui/settings/graph-extensions/ExtensionNodeShell.tsx` |
| **Grouped `tool-picker` Integration** | Wires Wave U1 `ProjectToolkitsMultiPicker` into `ToolkitsSelect` (`LLMNode`), `ProjectAgentPicker` into `AgentNode`, and `ProjectToolkitPicker` into `BaseToolNode` (`ToolkitNode` / `McpNode`) with search, grouped lists, Create new, MCP status, and EL-7085 disabled reasons. | Preserves YAML `tool_names` / `toolkit_name` / `tool` / `input_mapping` write semantics and strict `customFilterTypes` isolation (MCP vs non-MCP vs Application). | `apps/elitea-web/src/features/pipelines/ui/select/ToolkitsSelect.tsx`, `ui/nodes/{AgentNode,BaseToolNode}.tsx` |

## 2. Verification Summary

- **Typecheck:** `npm run typecheck` (`tsc --noEmit`) — **PASS** (`0` errors)
- **Lint:** `npm run lint -- --deny-warnings` (`oxlint`) — **PASS** (`0` warnings, `0` errors across 6,637 files)
- **Theme / Budgets / TestID / Layer / Dead Code / i18n / Upgrading Notes:**
  - `npm run theme-gate` — **PASS** (all 9 gates + `brandPack.contract.test.ts`)
  - `node scripts/check-budgets.mjs` — **PASS**
  - `node scripts/check-testid-namespace.mjs` — **PASS**
  - `node scripts/check-layer-cycle.mjs` — **PASS**
  - `node scripts/check-dead-code.mjs` (Knip) — **PASS** (`0` issues)
  - `node scripts/i18n-backfill.mjs --check` — **PASS** (8 new keys added to `src/shared/i18n/en.json`)
  - `node scripts/ci/check-upgrading-notes.mjs` — **PASS**
- **Vitest (`nice -n 10 npx vitest run --maxWorkers=2 src/features/pipelines/`):**
  - **272/272 test files passed** (`2,953 passed | 1 expected fail (2,954)`, `0` unexpected failures; the 1 expected fail is the pre-existing `DataTypeValueField.test.tsx:146` `it.fails` for issue #1930)
  - Includes new unit/integration tests in `FlowNodeConfigDrawer.test.tsx` (8 tests), updated `NodeCard.test.tsx` (6 tests), `ToolkitsSelect.test.tsx` (5 tests), and `FlowEditor.test.tsx` (19 tests)
- **Secret Scan:** `/usr/bin/grep` on `git diff origin/main..HEAD` — `0` hits
- **Deferred:** Real-browser Chrome / live-stack verification is deferred while Docker stacks are paused for host EKG builds; will be verified on Wave U2 integration spin-up.

## 3. Performance / Durability / Resilience / Security

- **Performance:** Pure React/MUI presentation and memoized selectors (`useStore(selectIsMiniCard)`, memoized `NodeCardView` / `FlowNodeConfigDrawer` / `useNodeYamlEditor`). Replacing expanded inline forms on canvas cards with compact view-only `NodeCardView` reduces canvas DOM size per node.
- **Durability:** N/A (client-side UI only; preserves all YAML document and `settings` persistence contracts verbatim).
- **Resilience:** Per-node YAML editing validates syntax, single-object shape, and compiler-legal unique IDs before mutating `yamlJsonObject`, surfacing inline `role="alert"` errors without corrupting graph state.
- **Security (`rules/security.md`):**
  - External documentation links (`NodeConfigPanelHeader` via `nodeDocs.constants.ts` and `NodeDeprecationBanner` via `TextWithLink`) use `target="_blank" rel="noopener noreferrer"` and static curated URLs only.
  - Per-node YAML parsing uses `js-yaml`'s safe `load()` (default safe schema in `js-yaml` v4; no `eval` or `dangerouslySetInnerHTML`).
  - All user-authored node names, admission messages, and summary strings render via React text nodes (auto-escaped).
  - No new dependencies, no secrets, no auth/permission bypasses.
