# Pipeline UI Wave U3: Workspace Shell, Docking Glue & On-Demand Resizable TestPanel (FU-283)

- **Date:** 2026-11-24
- **Branch:** `feat/fu283-u3-test-panel-shell` (based on `origin/main` `2df0a49cf`)
- **Reference UI:** `EliteaUI` `origin/main` (`CanvasToolbar.jsx`, `PipelineEditorWorkspace.jsx`, `PipelineTestRegion.jsx`, `TestPanel.jsx`, `TestPanelControls.jsx`, `EntityTestPanelShell.jsx`, `TestEntityButton.jsx`, `TestPanelCloseButton.jsx`, `TestPanelResizeHandle.jsx`, `useTestPanelWidth.hooks.js`, `pipelineReadiness.helpers.js`, `GeneralFormPanel.jsx`, `PipelineConfigurationForm.jsx`)
- **Scope:** Web-only (`apps/elitea-web`) Wave U3 Workspace Shell & On-Demand TestPanel, strictly disjoint from `ui-u3-node-polish`, `5d-2`, and `5e-2`.

## 1. Ported Business Behavior & Exact Anchors

| Area | Reference (`EliteaUI`) | `apps/elitea-web` Path | Status & Behavior |
|---|---|---|---|
| **Wave U2 docking glue** | `FlowEditor.jsx` (`stateRightOffset`), `StateDrawer.jsx`, `CanvasMiniMap.jsx` | `features/pipelines/ui/FlowEditor.tsx:277-284,352-375`, `features/pipelines/ui/useFlowEditorLifecycle.ts:231-241`, `features/pipelines/ui/state/StateDrawer.tsx:45-68` | `computeDockingOffsets` docks `StateDrawer` and `FlowEditorStateToggle` at `400px` (`+ extraRightOffsetPx`) whenever `FlowNodeConfigDrawer` is open, and `StateDrawer` reports its active width (`onWidthChange`) so `CanvasMiniMap` offsets by `drawerRightOffset + stateDrawerWidth` and never overlaps either drawer or the flex-sibling `TestPanel`. |
| **On-demand resizable `TestPanel` & `"Test Pipeline"` button (Gap #8)** | `CanvasToolbar.jsx`, `EntityTestPanelShell.jsx`, `TestPanel.jsx`, `TestPanelControls.jsx`, `TestEntityButton.jsx`, `TestPanelCloseButton.jsx`, `TestPanelResizeHandle.jsx`, `useTestPanelWidth.hooks.js`, `pipelineReadiness.helpers.js` | `features/pipelines/lib/flow-editor/helpers/pipelineReadiness.helpers.ts:1-141`, `features/pipelines/lib/hooks/useTestPanelWidth.ts:1-96`, `features/pipelines/ui/TestPipelineButton.tsx:1-38`, `features/pipelines/ui/EditorPanel.tsx:314-354`, `features/pipelines/ui/ChatPanel.tsx:1-297`, `features/pipelines/ui/ChatPanel.styles.ts:1-94`, `features/pipelines/ui/ConfigurationTab.tsx:162-399` | Right TestPanel now starts closed by default (`isTestPanelOpen = false`, stays mounted with `display: none` / `aria-hidden="true"` so live runs and conversations survive toggling) and is opened via the `"Test Pipeline"` toolbar button (`data-testid="pipeline-test-button"`), which hides while open. Disabled-reason tooltips cover unsaved pipeline (`Save the pipeline to test it`) and flow readiness (`Add and configure at least one node connected to End.`, `Set an entry point to test the pipeline.`, `Connect the entry point to End to test the pipeline.`, `Configure node "...": ...`). Resizable left edge (`<hr aria-orientation="vertical">`, 320px min, 373px default, 50% workspace max, persisted via `createStorage('local')` under `el.pipeline.testPanel.width`) and header controls (`ContextBudget`, `Clear`, `ViewRunHistoryButton`, vertical divider + `Close`). Restoring a run from Run History automatically opens the TestPanel. All replatform `PipelineTestChat` behavior (ephemeral run reset, Restore Test playback + non-spinning disabled composer `#3000`, `ExistingTestRuns`, `editor-test-clear`, `NodeRecoveryNotice`, `StaticPauseCard`, `DelegationNotices`) is strictly preserved. |
| **Left config pane order & auto-collapse (Gap #9)** | `PipelineConfigurationForm.jsx`, `GeneralFormPanel.jsx`, `PipelineEditorWorkspace.jsx` | `features/agents/ui/CreateAgentForm.tsx:194-205,235-244,342-358`, `pages/pipelines/ui/EditPipelineConfigurationPanel.tsx:112-145`, `features/pipelines/ui/GeneralFormPanel.tsx:55-120`, `features/pipelines/ui/ConfigurationTab.tsx:139-160`, `features/pipelines/ui/FlowWrapper.tsx:70-108` | Reordered left configuration accordions to `General -> Tools -> Welcome -> Starters -> Triggers & schedules -> Advanced -> Information` via backward-compatible optional `generalSlots.{afterGeneral,beforeAdvanced}` slots on `CreateAgentForm`, preserving `ApplicationMcpAccessToggle`, `OutputContractField`, and citations. Wired `onCanvasNodeInteraction` (`FlowEditor` -> `FlowWrapper` -> `EditorPanel` -> `ConfigurationTab` -> controlled `GeneralFormPanel`) to auto-collapse the left config pane on first node interaction while letting users re-expand it via `pipeline-config-collapse-button`. |
| **Header bar layout polish (Gap #10)** | `ApplicationTabBar.jsx`, `ApplicationControls.jsx` | `pages/pipelines/EditPipeline.tsx:74-75,345-364`, `pages/pipelines/ui/EditPipelineActions.tsx:142` | Aligned header toolbar ordering to `EditPipelineVersionBar` (Compare, Version select, Save As Version) -> `EditPipelineSaveBar` (Save, Cancel/Discard) -> `EditPipelineActions` (Chat, `⋮` kebab with Share / Fork / Export / Delete; Publish stays hidden) with uniform `0.5rem`/`0.75rem` spacing and zero backend API contract changes. |

## 2. Changed Files Inventory (26 files)

- **Production Web (16):**
  - `apps/elitea-web/src/features/agents/ui/CreateAgentForm.tsx`
  - `apps/elitea-web/src/features/pipelines/lib/flow-editor/helpers/pipelineReadiness.helpers.ts`
  - `apps/elitea-web/src/features/pipelines/lib/flow-editor/hooks/useResizableDrawer.ts`
  - `apps/elitea-web/src/features/pipelines/lib/hooks/useTestPanelWidth.ts`
  - `apps/elitea-web/src/features/pipelines/ui/ChatPanel.styles.ts`
  - `apps/elitea-web/src/features/pipelines/ui/ChatPanel.tsx`
  - `apps/elitea-web/src/features/pipelines/ui/ConfigurationTab.tsx`
  - `apps/elitea-web/src/features/pipelines/ui/EditorPanel.tsx`
  - `apps/elitea-web/src/features/pipelines/ui/FlowEditor.tsx`
  - `apps/elitea-web/src/features/pipelines/ui/FlowWrapper.tsx`
  - `apps/elitea-web/src/features/pipelines/ui/GeneralFormPanel.tsx`
  - `apps/elitea-web/src/features/pipelines/ui/TestPipelineButton.tsx`
  - `apps/elitea-web/src/features/pipelines/ui/state/StateDrawer.tsx`
  - `apps/elitea-web/src/features/pipelines/ui/useFlowEditorLifecycle.ts`
  - `apps/elitea-web/src/pages/pipelines/EditPipeline.tsx`
  - `apps/elitea-web/src/pages/pipelines/ui/EditPipelineActions.tsx`
  - `apps/elitea-web/src/pages/pipelines/ui/EditPipelineConfigurationPanel.tsx`
  - `apps/elitea-web/src/shared/i18n/en.json`
- **Tests (7):**
  - `apps/elitea-web/src/features/pipelines/lib/flow-editor/helpers/pipelineReadiness.helpers.test.ts`
  - `apps/elitea-web/src/features/pipelines/lib/hooks/useTestPanelWidth.test.ts`
  - `apps/elitea-web/src/features/pipelines/ui/ChatPanel.test.tsx`
  - `apps/elitea-web/src/features/pipelines/ui/ConfigurationTab.test.tsx`
  - `apps/elitea-web/src/features/pipelines/ui/EditorPanel.test.tsx`
  - `apps/elitea-web/src/features/pipelines/ui/FlowEditor.test.tsx`
  - `apps/elitea-web/src/pages/pipelines/ui/EditPipelineConfigurationPanel.test.tsx`
- **Docs (2):**
  - `services/elitea-worker-rust/docs/source-mapping/pipeline-ui-u3-test-shell-20261124.md`
  - `services/elitea-worker-rust/docs/source-mapping/README.md`

## 3. Deliberately Not Ported / Visual Snapshot Impact

- **Node cards, node panels, tool pickers, CreatePipeline, PipelineListPanel:** Owned by sibling `ui-u3-node-polish`; zero edits here.
- **Editor Notes accordion:** Withheld because backend `application_versions` has no `notes` column (preserved documented contract).
- **Visual snapshot baseline note:** Because the right `TestPanel` is now **closed by default** on the pipeline editor (matching EliteaUI `EL-7378`), and the left configuration pane reorders `Tools` and `Triggers & schedules` ahead of `Advanced`, Linux Playwright visual snapshots covering the Edit Pipeline page will reflect the wider default canvas and `"Test Pipeline"` toolbar button. Linux PNG snapshots were **not** regenerated on macOS per policy and should be refreshed in Linux CI if visual baseline diffs are expected.
- **Real-browser Chrome proof:** Deferred to integrated Wave U3 stack verification (Web-only worktree; no Docker/Rust/Go stacks started).

## 4. Verification & Observed Results

Executed in `apps/elitea-web` (Node `v24.19.0`):

- **Full `src/features/pipelines/` Vitest (`nice -n 10 npx vitest run --maxWorkers=2 src/features/pipelines/` + targeted re-run of fixed `ChatPanel.test.tsx` & `FlowEditor.test.tsx`):** **`275 passed (275)`** test files, **`3,005 passed | 1 expected fail (3,006)`** (`0` unexpected failures; the 1 expected fail is the pre-existing `DataTypeValueField.test.tsx:146` `it.fails` for #1930).
- **Full `src/pages/pipelines/` + `CreateAgentForm.test.tsx` Vitest:** **`37 passed (37)`** test files, **`331 passed (331)`** tests (`0` failures, including `EditPipeline.test.tsx`, `CreatePipeline.test.tsx`, `EditPipelineConfigurationPanel.test.tsx`, `PipelineTestChat.test.tsx`, `PipelineTestChatCodeDebug.test.tsx`, `PipelineTestChatIdle.test.tsx`, `EditPipelineActions.test.tsx`).
- **TypeScript (`npm run typecheck`):** `tsc --noEmit` **PASS** (`0` errors).
- **Lint (`npm run lint -- --deny-warnings`):** `oxlint` **PASS** (`0` warnings, `0` errors across `6,672` files).
- **Theme gate (`npm run theme-gate`):** **PASS** (all 9 checks + `brandPack.contract.test.ts` `5/5`).
- **Budgets (`node scripts/check-budgets.mjs`):** **OK** (`7,481` files within §3.5 budgets).
- **Testid namespace (`node scripts/check-testid-namespace.mjs`):** **OK** (`2,946` test ids).
- **Layer cycles (`node scripts/check-layer-cycle.mjs`):** **OK** (`5,984` modules, `0` errors, `0` warnings).
- **Dead code (`node scripts/check-dead-code.mjs`):** **OK** (Knip: `0` issues).
- **i18n (`node scripts/i18n-backfill.mjs --check`):** **OK** (`9` new keys backfilled to `src/shared/i18n/en.json`).
- **Dependency audit (`npm audit --audit-level=high`):** **PASS** (`0` high, `0` critical; `21` pre-existing moderate dev-dep advisories; `0` dependencies added).
- **Secret scan (`git diff` grep):** **PASS** (`0` hits).

## 5. Performance, Durability, Resilience, Recovery & Security

- **Performance:** Pure client-side React layout and memoized graph-reachability BFS (`isEndReachable` in `pipelineReadiness.helpers.ts:43-64`, O(V+E) with visited set preventing cycle loops); resize dragging uses RAF-throttled DOM width updates via `useResizableDrawer.ts:46-71`; zero extra network requests on page load because `PipelineTestChat` stays lazy until interacted with.
- **Durability:** TestPanel width persists under `createStorage('local')` (`el.pipeline.testPanel.width`, `useTestPanelWidth.ts:16-35`), cleared automatically on logout; keeping `ChatPanel` mounted while closed preserves active streaming runs and conversations across panel toggles.
- **Resilience:** `getTestBlockReason` catches malformed YAML parse errors (`pipelineReadiness.helpers.ts:95-101`), `useTestPanelWidth` clamps stored/dragged widths to `[320px, max(320px, 50% workspace)]` and degrades gracefully if `localStorage` or `ResizeObserver` is unavailable.
- **Recovery guarantees:** N/A — pure Web UI shell, layout, and docking changes; no Main, Worker, Sandbox, NATS, PostgreSQL, or LLM checkpoint/recovery path is modified.
- **Security (`rules/security.md`):**
  - Trust boundaries / Authorization / Egress / Sandbox / AuthN / Cross-layer / Delegation: N/A (Web presentation/shell only; no routes, queries, or wire contracts changed; Publish stays hidden on pipelines).
  - Input/parsing/amplification: `isEndReachable` tracks `visited` nodes to guarantee O(V+E) termination on cyclic graphs (`pipelineReadiness.helpers.ts:51-62`).
  - Injection/XSS: All tooltips, labels, and node issue messages render as plain React text nodes (no `dangerouslySetInnerHTML`, no raw HTML/URL sinks).
  - Secrets & Supply chain: `0` secret scan hits, `0` new dependencies, `npm audit --audit-level=high` = `0` high/critical.

## 6. Fixtures & Follow-ups

- **Fixtures:** Unit and component tests use MSW handlers and Testing Library (`EditPipelineConfigurationPanel.test.tsx`, `ConfigurationTab.test.tsx`, `ChatPanel.test.tsx`, `EditorPanel.test.tsx`, `FlowEditor.test.tsx`, `pipelineReadiness.helpers.test.ts`, `useTestPanelWidth.test.ts`).
- **Follow-ups:** Refresh Linux Playwright visual snapshots for the Edit Pipeline page once Wave U3 PRs land, and perform integrated real-browser verification on a disposable stack.
