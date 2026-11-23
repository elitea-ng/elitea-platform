# Pipeline UI Wave U2 Part B: Workspace Shell, Canvas Controls, Full-Screen YAML, State Drawer & EL-3734 Timeline (FU-283)

- **Date:** 2026-11-23
- **Branch:** `feat/fu283-u2-workspace-shell` (based on `54718237f`)
- **Reference UI:** `EliteaUI` `origin/main` (`46d85cda7` / `ec684f56b`, `#1585`)
- **Scope:** Pipeline UI Wave U2 Part B workspace shell integration, keeping strict file disjointness from `ui-u2-node-panels` (`ui/nodes/*`, `ui/node-card-v2/*`, `ui/node-config-drawer/*`, `ui/tool-picker/*`, `ui/select/*` untouched).

## 1. Ported Business Behavior & Exact Anchors

| Area | Reference (`EliteaUI`) | `apps/elitea-web` Path | Status & Behavior |
|---|---|---|---|
| **Add-node palette & empty canvas** | `NodeTypeSelector.jsx`, `FlowEmptyState.jsx` (`EL-7154`) | `features/pipelines/ui/AddNodeMenu.tsx:1-29`, `features/pipelines/ui/FlowEditor.tsx:116-123,359-364` | Wired to `AddNodeButton` (`NodeTypeSelector`, all 14 compiler-admitted node types) and `FlowEmptyState` (hiding `End` while 0 user nodes exist); verified end-to-end in `FlowEditor.test.tsx`. |
| **Canvas controls & minimap** | `CanvasControls.jsx`, `CanvasMiniMap.jsx` (`EL-7160`) | `features/pipelines/ui/FlowEditorCanvasControls.tsx:1-92`, `features/pipelines/ui/FlowEditor.tsx:125-127,135-137,255-263,343-355`, `features/pipelines/ui/useFlowEditorNodeOperations.ts:184-190,261-304` | Replaced legacy `StyledFlowControls` ("Toggle cards size" + single "Auto-arrange") with Wave U2 `CanvasControls` (Zoom +/-, Fit view, Lock/Unlock canvas interactivity matching `EliteaUI` `FlowEditor.jsx:1070-1072`, TB/LR Auto-arrange menu, and Minimap toggle persisted via `createStorage('local')` under `el.pipeline.flow.minimapVisible`) + `CanvasMiniMap`. Removed unused `StyledFlowControls` from `FlowEditor.styles.ts`. |
| **Duplicate node action** | `FlowEditor.jsx:490-565` (`EL-7161`) | `features/pipelines/lib/duplicateNode.ts:1-132`, `features/pipelines/ui/useFlowEditorNodeOperations.ts:192-259`, `features/pipelines/lib/flow-editor/flowEditorContext.ts:42`, `features/pipelines/ui/FlowEditorProvider.tsx:48,76`, `features/pipelines/ui/FlowEditor.tsx:99,207-235,317` | Wired `lib/duplicateNode.ts` into `useFlowEditorNodeOperations` with synchronous pending-state tracking for rapid double-duplicate calls, `MAX_NODE_ID_BYTES` (128) ID clamping, `versionTools` name-collision avoidance, read-only `disabled` guard on both context `handleDuplicateNode` and imperative `FlowEditorHandle.onDuplicateNode`, transient `data.isPerforming` stripping, and compiler-admitted connection reset (`transition: 'END'` for `Printer`, `default_output: 'END'` for `Router`/`Decision`, `{ approve: 'END', reject: 'END' }` for `Hitl`; verified against `collectGraphAdmissionIssues`). |
| **Flow \| YAML toolbar & full-screen YAML** | `CanvasToolbar.jsx`, `YamlCodeEditor.jsx` (`EL-7158`) | `features/pipelines/ui/EditorPanel.tsx:290-296,340-355`, `features/pipelines/ui/YamlCodeEditor.tsx:116-304` | Updated mode toggle label from `Yaml` to `YAML` (`features.pipelines.editorPanel.mode.{flow,yaml}`), moved Copy YAML into `YamlCodeEditor`'s action bar alongside Full screen view (`BaseModal` `variant="complex"` `fullscreen`), attached refs to both inline and full-screen CodeMirror editors to flush un-debounced edits on open/close transitions and copy live editor text. |
| **State drawer alignment** | `StateDrawer.jsx`, `StateVariableList.jsx` | `features/pipelines/lib/flow-editor/constants/flowEditor.constants.ts:28-59`, `features/pipelines/lib/flow-editor/helpers/state.helpers.ts:111-120`, `features/pipelines/ui/state/StateDrawer.tsx:44-84,122-186`, `features/pipelines/ui/state/StateVariableList.tsx:159-231` | Added `rightOffset` prop on `StateDrawer` to dock cleanly left of the 400px `NodeConfigDrawer` (asserted via `getComputedStyle(drawer).right === '400px'`), added `tool_outcomes` and `last_tool_outcome` opt-in fixed rows (`type: 'dict'` via `DefaultStateTypes` + `isDefaultVariableEnabled`), and preserved all replatform `ReservedStateKeys` compiler validation rules. |
| **EL-3734 `RunStateDialog` timeline** | `RunStateDialog.jsx` + `timeline/*` (`#1585`) | `features/pipelines/ui/state/RunStateDialog.tsx:38-248`, `features/pipelines/ui/state/runStateValues.ts:1-81`, `features/pipelines/ui/state/RunStateDialog.parts.tsx:1-190`, `features/pipelines/ui/state/RunStateDialog.status.tsx:1-54`, `features/pipelines/ui/state/RunStateDialog.styles.ts:1-180` | Replaced flat MUI `Stepper` with Wave U2 `TimeLine` (`ui/run-state-timeline/*` + `lib/run-state-timeline/*`), reusing `findPreviousScopedStep` / `findLastOuterStepIndex` so outer-step `Before` never leaks nested child state, `#6883` `Final state` resolves on `lastOuterIndex` scanning outer steps only, mounted `selectedStep` resets on `data.label` change and clamps when `steps` shrinks, and 5b-1 D12 `node_errors` (`"Failed · continued"` + support refs) render safely. Removed superseded legacy exports (`ProcessConnector`, `ProcessStepIcon`, `InProgressIndicator`, `StatusIndicatorRow`, `continuedFailureSteps`, and unused stepper styles). |

## 2. Changed Files Inventory (34 files, strictly disjoint from `ui-u2-node-panels`)

- **Core UI & Helpers (15):** `lib/duplicateNode.ts`, `lib/flow-editor/constants/flowEditor.constants.ts`, `lib/flow-editor/flowEditorContext.ts`, `lib/flow-editor/helpers/state.helpers.ts`, `ui/EditorPanel.tsx`, `ui/FlowEditor.styles.ts`, `ui/FlowEditor.tsx`, `ui/FlowEditorCanvasControls.tsx`, `ui/FlowEditorProvider.tsx`, `ui/YamlCodeEditor.tsx`, `ui/state/RunStateDialog.parts.tsx`, `ui/state/RunStateDialog.status.tsx`, `ui/state/RunStateDialog.styles.ts`, `ui/state/RunStateDialog.tsx`, `ui/state/StateDrawer.tsx`, `ui/state/StateVariableList.tsx`, `ui/state/runStateValues.ts`, `ui/useFlowEditorNodeOperations.ts`, `shared/i18n/en.json` (19 production files).
- **Tests (13):** `lib/duplicateNode.test.ts`, `lib/pipelineNumericNodeIds.integration.test.tsx`, `ui/EditorPanel.test.tsx`, `ui/FlowEditor.test.tsx`, `ui/FlowEditorCanvasControls.test.tsx`, `ui/YamlCodeEditor.test.tsx`, `ui/state/RunStateDialog.parts.test.tsx`, `ui/state/RunStateDialog.status.test.tsx`, `ui/state/RunStateDialog.styles.test.ts`, `ui/state/RunStateDialog.test.tsx`, `ui/state/StateDrawer.test.tsx`, `ui/state/runStateValues.test.ts`, `ui/useFlowEditorNodeOperations.test.ts`.
- **Docs (2):** `services/elitea-worker-rust/docs/source-mapping/pipeline-ui-u2-workspace-shell-20261123.md`, `services/elitea-worker-rust/docs/source-mapping/README.md`.

## 3. Deliberately Not Ported / Follow-Ups

- **Node Panel & View-Only Cards (`ui/nodes/*`, `ui/node-card-v2/*`, `ui/node-config-drawer/*`, `ui/tool-picker/*`, `ui/select/*`):** Owned by parallel sibling `ui-u2-node-panels`; zero edits here so both PRs merge cleanly. Once merged, `FlowEditor` can pass `rightOffset={400}` to `StateDrawer` and `CanvasMiniMap` when `NodeConfigDrawer` is open.
- **Canvas lock scope (`isCanvasLocked`):** Gates `nodesDraggable`, `nodesConnectable`, and `edgesReconnectable` matching `EliteaUI` `FlowEditor.jsx:1070-1072`; delete/duplicate/arrange remain gated on `disabled` / `canArrange`.
- **On-demand TestPanel / Run History / Schedules / Webhooks:** Deferred to Wave U2 follow-on / cross-layer track per `WAVE-U1-PLAN.md` §1 (Q4/Q5).
- **Real-browser Chrome proof:** Deferred to integrated Wave U2 stack verification (Web-only worktree; no Docker/Rust/Go stacks started per throttling rules).

## 4. Verification & Observed Results

- **Full pipelines Vitest (`nice -n 10 npx vitest run --maxWorkers=2 src/features/pipelines/`):** **`271 passed (271)`** test files, **`2,954 passed | 1 expected fail (2,955)`** (`0` unexpected failures; the 1 expected fail is the pre-existing `DataTypeValueField.test.tsx:146` `it.fails` for issue #1930).
- **TypeScript (`npm run typecheck`):** `tsc --noEmit` PASS (`0` errors).
- **Lint (`npm run lint -- --deny-warnings`):** `oxlint` PASS (`0` warnings, `0` errors across 6,631 files).
- **Theme gate (`npm run theme-gate`):** PASS (all 9 checks + brand pack contract).
- **Budgets (`node scripts/check-budgets.mjs`):** OK (7,408 files within §3.5 budgets).
- **Testid namespace (`node scripts/check-testid-namespace.mjs`):** OK (2,912 testids).
- **Layer cycles (`node scripts/check-layer-cycle.mjs`):** OK (`0` errors, `0` warnings).
- **i18n (`node scripts/i18n-backfill.mjs --check`):** OK (5 new keys backfilled to `src/shared/i18n/en.json`).
- **Dead code (`node scripts/check-dead-code.mjs`):** OK (Knip: `0` issues).
- **Upgrading notes (`node scripts/ci/check-upgrading-notes.mjs`):** OK.
- **Dependency audit (`npm audit --audit-level=high`):** `0` high, `0` critical (`21` pre-existing moderate; `0` dependencies added).
- **Secret scan (`git diff 54718237f` grep):** `0` hits.

## 5. Performance, Durability, Resilience & Security

- **Performance:** Purely client-side React/xyflow integration; memoized callbacks/selectors (`useMemo`/`useCallback`) preserve render bounds and keep `FlowEditor.tsx` (394 lines) and `useFlowEditorNodeOperations.ts` within §3.5 complexity (`<= 12`) and length (`<= 400`) budgets.
- **Durability & Recovery:** Minimap visibility uses `createStorage('local')` (`@/shared/lib/storage`) under the `el.` namespace so logout `clearNamespace()` wipes it cleanly; `YamlCodeEditor` flushes live CodeMirror buffer via `getCode()` on open/close transitions so edits within the 30ms debounce window are never dropped; no backend recovery state or wire contracts modified (N/A for Worker/Main recovery rows).
- **Resilience:** `onDuplicateNode` aborts cleanly when `setYamlJsonObject` refuses a write (`stored === false`), tracks synchronous pending writes across rapid calls before React commits, and `RunStateDialog` clamps/resets `selectedStep` when `steps` shrinks or `data.label` changes.
- **Security (`rules/security.md`):** UI read-only `disabled` guard enforced across both context and imperative `onDuplicateNode`, `YamlCodeEditor`, and `StateDrawer`; `node_errors`, `support_ref`, timeline labels, and state values render as plain React text nodes (no `dangerouslySetInnerHTML`, no raw HTML/URL sinks); `tool_outcomes`/`last_tool_outcome` are outside `ReservedStateKeys` and preserve compiler-reserved key validation; zero new dependencies; secret scan `0` hits.
