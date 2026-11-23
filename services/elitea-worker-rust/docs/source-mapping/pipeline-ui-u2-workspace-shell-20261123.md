# Pipeline UI Wave U2 Part B: Workspace Shell, Canvas Controls, Full-Screen YAML, State Drawer & EL-3734 Timeline (FU-283)

- **Date:** 2026-11-23
- **Branch:** `feat/fu283-u2-workspace-shell` (based on `54718237f`)
- **Reference UI:** `EliteaUI` `origin/main` (`46d85cda7` / `ec684f56b`, `#1585`)
- **Scope:** Pipeline UI Wave U2 Part B workspace shell integration, keeping strict file disjointness from `ui-u2-node-panels` (`ui/nodes/*`, `ui/node-card-v2/*`, `ui/node-config-drawer/*`, `ui/tool-picker/*`, `ui/select/*` untouched).

## 1. Ported Business Behavior & Exact Anchors

| Area | Reference (`EliteaUI`) | `apps/elitea-web` Path | Status & Behavior |
|---|---|---|---|
| **Add-node palette & empty canvas** | `NodeTypeSelector.jsx`, `FlowEmptyState.jsx` (`EL-7154`) | `features/pipelines/ui/AddNodeMenu.tsx`, `features/pipelines/ui/FlowEditor.tsx:108-115,342-347` | Already wired to `AddNodeButton` (`NodeTypeSelector`, all 14 compiler-admitted node types) and `FlowEmptyState` (hiding `End` while 0 user nodes exist); verified end-to-end in `FlowEditor.test.tsx`. |
| **Canvas controls & minimap** | `CanvasControls.jsx`, `CanvasMiniMap.jsx` (`EL-7160`) | `features/pipelines/ui/FlowEditorCanvasControls.tsx:1-86`, `features/pipelines/ui/FlowEditor.tsx:121-130,253-267,334-340`, `features/pipelines/ui/useFlowEditorNodeOperations.ts:184-260` | Replaced legacy `StyledFlowControls` ("Toggle cards size" + single "Auto-arrange") with Wave U2 `CanvasControls` (Zoom +/-, Fit view, Lock/Unlock canvas interactivity, TB/LR Auto-arrange menu, and Minimap toggle persisted via `createStorage('local')` under `el.pipeline.flow.minimapVisible`) + `CanvasMiniMap`. Removed unused `StyledFlowControls` from `FlowEditor.styles.ts`. |
| **Duplicate node action** | `FlowEditor.jsx:490-565` (`EL-7161`) | `features/pipelines/ui/useFlowEditorNodeOperations.ts:192-219`, `features/pipelines/lib/flow-editor/flowEditorContext.ts:42`, `features/pipelines/ui/FlowEditorProvider.tsx:48,75`, `features/pipelines/ui/FlowEditor.tsx:99,268-274` | Wired `lib/duplicateNode.ts` (`canDuplicateNodeType`, `getDuplicateNodeId`, `duplicateYamlNode`, `duplicateFlowNode`) into `useFlowEditorNodeOperations`, exposed via `FlowEditorContext.handleDuplicateNode` and `FlowEditorHandle.onDuplicateNode`. Clears outgoing `transition`, `settings.error_transition`, Router/Decision/HITL routes, and refuses deprecated/`END`/`ghost` nodes. |
| **Flow \| YAML toolbar & full-screen YAML** | `CanvasToolbar.jsx`, `YamlCodeEditor.jsx` (`EL-7158`) | `features/pipelines/ui/EditorPanel.tsx:290-296`, `features/pipelines/ui/YamlCodeEditor.tsx:116-248` | Updated mode toggle label from `Yaml` to `YAML` (`features.pipelines.editorPanel.mode.{flow,yaml}`), moved Copy YAML into `YamlCodeEditor`'s action bar alongside Full screen view (`BaseModal` `variant="complex"` `fullscreen`), preserving CodeMirror 6 YAML syntax highlighting, linter `.error_yaml_code` styling, and `ref.getCode()` draft retention. |
| **State drawer alignment** | `StateDrawer.jsx`, `StateVariableList.jsx` | `features/pipelines/lib/flow-editor/constants/flowEditor.constants.ts:28-56`, `features/pipelines/lib/flow-editor/helpers/state.helpers.ts:111-120`, `features/pipelines/ui/state/StateDrawer.tsx:44-80,120-185`, `features/pipelines/ui/state/StateVariableList.tsx:159-231` | Added `rightOffset` prop on `StateDrawer` to dock cleanly left of the 400px `NodeConfigDrawer`, added `tool_outcomes` and `last_tool_outcome` opt-in fixed rows (`type: 'dict'` via `DefaultStateTypes` + `isDefaultVariableEnabled`), and preserved all replatform `ReservedStateKeys` compiler validation rules. |
| **EL-3734 `RunStateDialog` timeline** | `RunStateDialog.jsx` + `timeline/*` (`#1585`) | `features/pipelines/ui/state/RunStateDialog.tsx:38-255`, `features/pipelines/ui/state/runStateValues.ts:35-37`, `features/pipelines/ui/state/RunStateDialog.parts.tsx`, `features/pipelines/ui/state/RunStateDialog.styles.ts` | Replaced flat MUI `Stepper` with Wave U2 `TimeLine` (`ui/run-state-timeline/*` + `lib/run-state-timeline/*`), supporting grouped nested/Map/Parallel/iteration runs (`groupedTimeline`), `Expand all` switch, scoped Before/After values for nested steps, `#6883` `Final state` on terminal runs, and safe React-rendered 5b-1 D12 `node_errors` (`"Failed · continued"` + support refs). Removed dead legacy `ProcessConnector` and unused stepper styles. |

## 2. Verification & Observed Results

- **Targeted Vitest (`--maxWorkers=2`, 9 files):** `9 passed (9)`, `132 passed (132)` (`pipelineNumericNodeIds.integration.test.tsx`, `FlowEditor.test.tsx`, `FlowEditorCanvasControls.test.tsx`, `EditorPanel.test.tsx`, `YamlCodeEditor.test.tsx`, `StateDrawer.test.tsx`, `StateVariableList.test.tsx`, `RunStateDialog.test.tsx`, `useFlowEditorNodeOperations.test.ts`).
- **Full pipelines Vitest (`--maxWorkers=2`, `src/features/pipelines/`):** 269/271 files passed on initial full run (`2,959 passed`, `1 expected fail`), with the 2 remaining assertion updates (`pipelineNumericNodeIds.integration.test.tsx` `'YAML'` label and `FlowEditor.test.tsx` `mockClear()`) verified green in the 9-file rerun above (`132/132`).
- **TypeScript (`npm run typecheck`):** `tsc --noEmit` passed (0 errors).
- **Lint (`npm run lint -- --deny-warnings`):** `oxlint` passed (`0 warnings, 0 errors` across 6,631 files).
- **Theme gate (`npm run theme-gate`):** PASS (all 9 checks + brand pack contract).
- **Budgets (`node scripts/check-budgets.mjs`):** OK (7,408 files within §3.5 budgets).
- **Testid namespace (`node scripts/check-testid-namespace.mjs`):** OK (2,912 testids).
- **Layer cycles (`node scripts/check-layer-cycle.mjs`):** OK (0 errors, 0 warnings).
- **i18n (`node scripts/i18n-backfill.mjs --check`):** OK (5 new keys backfilled to `src/shared/i18n/en.json`).
- **Dead code (`node scripts/check-dead-code.mjs`):** OK (Knip found 0 issues).
- **Upgrading notes (`node scripts/ci/check-upgrading-notes.mjs`):** OK.
- **Secret scan (`git diff` grep):** `0` hits.
- **Real-browser Chrome proof:** Deferred to integrated Wave U2 stack verification (Web-only worktree; no Docker/Rust/Go stacks started per throttling rules).

## 3. Performance, Durability, Resilience & Security

- **Performance:** Purely client-side React/xyflow integration; memoized callbacks/selectors (`useMemo`/`useCallback`) preserve render bounds and keep `FlowEditor.tsx` within §3.5 complexity (`<= 12`) and length (`<= 400`) budgets.
- **Durability:** Minimap visibility uses `createStorage('local')` (`@/shared/lib/storage`) under the `el.` namespace so logout `clearNamespace()` wipes it cleanly (no raw `localStorage` leaks); `YamlCodeEditor` retains the live `CodeMirrorEditorHandle` ref so edits flush before Flow/YAML tab switches.
- **Resilience:** `onDuplicateNode` aborts cleanly when `setYamlJsonObject` refuses a write (`stored === false`), and `RunStateDialog` falls back to flat `data.timeline` when `data.groupedTimeline` is absent.
- **Security:** `node_errors` messages and support refs in `TimeLine` / `TimeLineContinuedFailureSummary` render as plain React text nodes (no `dangerouslySetInnerHTML`); `tool_outcomes` / `last_tool_outcome` are outside `ReservedStateKeys` and preserve compiler-reserved key validation; zero new dependencies added; secret scan 0 hits.
