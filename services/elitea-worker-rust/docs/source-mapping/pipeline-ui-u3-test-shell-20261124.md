# Pipeline UI Wave U3: Workspace Shell, Docking Glue & On-Demand Resizable TestPanel (FU-283)

- **Date:** 2026-11-24
- **Branch:** `feat/fu283-u3-test-panel-shell` (based on `origin/main` `2df0a49cf`)
- **Reference UI:** `EliteaUI` `origin/main` (`CanvasToolbar.jsx`, `PipelineEditorWorkspace.jsx`, `PipelineTestRegion.jsx`, `TestPanel.jsx`, `TestPanelControls.jsx`, `EntityTestPanelShell.jsx`, `TestEntityButton.jsx`, `TestPanelCloseButton.jsx`, `TestPanelResizeHandle.jsx`, `useTestPanelWidth.hooks.js`, `pipelineReadiness.helpers.js`, `GeneralFormPanel.jsx`, `PipelineConfigurationForm.jsx`)
- **Scope:** Web-only (`apps/elitea-web`) Wave U3 Workspace Shell & On-Demand TestPanel, strictly disjoint from `ui-u3-node-polish`, `5d-2`, and `5e-2`.

## 1. Ported Business Behavior & Exact Anchors

| Area | Reference (`EliteaUI`) | `apps/elitea-web` Path | Status & Behavior |
|---|---|---|---|
| **Wave U2 docking glue** | `FlowEditor.jsx` (`stateRightOffset`), `StateDrawer.jsx`, `CanvasMiniMap.jsx` | `features/pipelines/ui/FlowEditor.tsx:198-206,277-285,352-375`, `features/pipelines/ui/useFlowEditorLifecycle.ts:231-241`, `features/pipelines/ui/state/StateDrawer.tsx:45-68` | `computeDockingOffsets` docks `StateDrawer` and `FlowEditorStateToggle` at `400px` whenever `FlowNodeConfigDrawer` is open, and `StateDrawer` reports its active width (`onWidthChange`) so `CanvasMiniMap` offsets by `drawerRightOffset + stateDrawerWidth` and never overlaps either drawer or the flex-sibling `TestPanel`. `handleAddNode` enforces the read-only `safeDisabled` guard (`SEC-U3-01`). |
| **On-demand resizable `TestPanel` & `"Test Pipeline"` button (Gap #8)** | `CanvasToolbar.jsx`, `EntityTestPanelShell.jsx`, `TestPanel.jsx`, `TestPanelControls.jsx`, `TestEntityButton.jsx`, `TestPanelCloseButton.jsx`, `TestPanelResizeHandle.jsx`, `useTestPanelWidth.hooks.js`, `pipelineReadiness.helpers.js` | `features/pipelines/lib/flow-editor/helpers/pipelineReadiness.helpers.ts:1-158`, `features/pipelines/lib/hooks/useTestPanelWidth.ts:1-101`, `features/pipelines/ui/TestPipelineButton.tsx:1-38`, `features/pipelines/ui/EditorPanel.tsx:316-358`, `features/pipelines/ui/ChatPanel.tsx:1-306`, `features/pipelines/ui/ChatPanel.styles.ts:1-94`, `features/pipelines/ui/ConfigurationTab.tsx:139-396` | Right TestPanel starts closed by default (`isTestPanelOpen = false`, stays mounted with `display: none` / `aria-hidden="true"` so live runs and conversations survive toggling) and is opened via `"Test Pipeline"` (`data-testid="pipeline-test-button"`), which hides while open. Disabled-reason tooltips cover unsaved pipeline (`Save the pipeline to test it`), flow readiness (`Add and configure at least one node connected to End.`, `Set an entry point to test the pipeline.`, `Connect the entry point to End to test the pipeline.`), and blocking node admission issues via `collectBlockingNodeIssuesById` (`Configure node "...": ...`) with `normalizePipelineNodeIdentifiers` and `Object.hasOwn` lookup guards (`SEC-U3-02`). `ViewRunHistoryButton` is rendered in `EditorPanel` toolbar while closed and inside `ChatPanelHeader` (`Run History -> Clear -> Close`) while open. Resizable left edge (`<hr aria-orientation="vertical">`, `320px` min, `373px` default, `50%` workspace max) clamps synchronously on initial read and write (`SEC-U3-03`) under `el.pipeline.testPanel.width`. Public read-only views (`ViewMode.Public`) disable `EditorPanel` and withhold `"Test Pipeline"`, `ChatPanel`, and Run History (`SEC-U3-01`). All replatform `PipelineTestChat` behavior (ephemeral run reset, Restore Test playback + non-spinning disabled composer `#3000`, `ExistingTestRuns`, `editor-test-clear`, `NodeRecoveryNotice`, `StaticPauseCard`, `DelegationNotices`) is preserved. |
| **Left config pane order & auto-collapse (Gap #9)** | `PipelineConfigurationForm.jsx`, `GeneralFormPanel.jsx`, `PipelineEditorWorkspace.jsx` | `features/agents/ui/CreateAgentForm.tsx:194-205,237-253,349-367`, `pages/pipelines/ui/EditPipelineConfigurationPanel.tsx:112-145`, `features/pipelines/ui/GeneralFormPanel.tsx:55-120`, `features/pipelines/ui/ConfigurationTab.tsx:139-165`, `features/pipelines/ui/FlowWrapper.tsx:70-108` | Reordered left configuration accordions to `General -> Tools -> Welcome -> Starters -> Triggers & schedules -> Advanced -> Information` via backward-compatible optional `generalSlots.{afterGeneral,beforeAdvanced}` slots (wrapped with `marginTop: '1rem'` only when non-null) on `CreateAgentForm`, preserving `ApplicationMcpAccessToggle`, `OutputContractField`, and citations. Wired `onCanvasNodeInteraction` (`FlowEditor` -> `FlowWrapper` -> `EditorPanel` -> `ConfigurationTab` -> controlled `GeneralFormPanel`) using `userChoice ?? foldGeneral` so first node interaction auto-collapses the left pane while users can always expand/collapse manually even when `foldGeneral` is `true`. |
| **Header bar layout polish (Gap #10)** | `ApplicationTabBar.jsx`, `ApplicationControls.jsx` | `pages/pipelines/EditPipeline.tsx:74-78,345-384`, `pages/pipelines/ui/EditPipelineActions.tsx:142` | Aligned header toolbar ordering to `EditPipelineVersionBar` (Compare, Version select, Save As Version) -> `EditPipelineSaveBar` (Save, Cancel/Discard) -> `EditPipelineActions` (Chat, `⋮` kebab with Share / Fork / Export / Delete; Publish stays hidden) with uniform spacing, propagated `viewMode` to `ConfigurationTab`, and made zero backend API contract changes. |

## 2. Changed Files Inventory (27 files)

- **Production Web (18):**
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
  - `apps/elitea-web/src/pages/pipelines/EditPipeline.test.tsx`
  - `apps/elitea-web/src/pages/pipelines/ui/EditPipelineConfigurationPanel.test.tsx`
- **Docs (2):**
  - `services/elitea-worker-rust/docs/source-mapping/pipeline-ui-u3-test-shell-20261124.md`
  - `services/elitea-worker-rust/docs/source-mapping/README.md`

*(Total: 28 files including `EditPipeline.test.tsx`.)*

## 3. Deliberately Not Ported / Visual Snapshot Impact

- **Node cards, node panels, tool pickers, CreatePipeline, PipelineListPanel:** Owned by sibling `ui-u3-node-polish`; zero edits here.
- **Editor Notes accordion:** Withheld because backend `application_versions` has no `notes` column (preserved documented contract).
- **Visual snapshot baseline note:** Because the right `TestPanel` is now **closed by default** on the pipeline editor (matching EliteaUI `EL-7378`), and the left configuration pane reorders `Tools` and `Triggers & schedules` ahead of `Advanced`, Linux Playwright visual snapshots covering the Edit Pipeline page will reflect the wider default canvas and `"Test Pipeline"` toolbar button. Linux PNG snapshots were **not** regenerated on macOS per policy and should be refreshed in Linux CI if visual baseline diffs are expected.
- **Real-browser Chrome proof:** Deferred to integrated Wave U3 stack verification (Web-only worktree; no Docker/Rust/Go stacks started).

## 4. Verification & Observed Results

Executed in `apps/elitea-web` (Node `v24.19.0`):

- **Targeted & delta Vitest suite (12 files: `pipelineReadiness.helpers`, `useTestPanelWidth`, `ChatPanel`, `ConfigurationTab`, `EditorPanel`, `FlowEditor`, `GeneralFormPanel`, `EditPipeline`, `EditPipelineConfigurationPanel`, `PipelineTestChat`, `CreatePipeline`, `CreateAgentForm`):** **`12/12` files, `198/198` tests PASS**.
- **Full `src/features/pipelines/` Vitest:** **`275 passed (275)`** test files, **`3,005 passed | 1 expected fail (3,006)`** (`0` unexpected failures; the 1 expected fail is the pre-existing `DataTypeValueField.test.tsx:146` `it.fails` for #1930).
- **Full `src/pages/pipelines/` + `CreateAgentForm.test.tsx` Vitest:** **`37 passed (37)`** test files, **`331 passed (331)`** tests.
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

- **Performance:** Pure client-side React layout and memoized graph-reachability BFS (`isEndReachable` in `pipelineReadiness.helpers.ts:44-66`, O(V+E) with visited set preventing cycle loops); resize dragging uses RAF-throttled DOM width updates via `useResizableDrawer.ts:46-71`; zero extra network requests on page load because `PipelineTestChat` stays lazy until interacted with.
- **Durability:** TestPanel width persists under `createStorage('local')` (`el.pipeline.testPanel.width`, `useTestPanelWidth.ts:18-41`), cleared automatically on logout; keeping `ChatPanel` mounted while closed preserves active streaming runs and conversations across panel toggles.
- **Resilience:** `getTestBlockReason` catches malformed YAML parse errors (`pipelineReadiness.helpers.ts:96-102`), `useTestPanelWidth` clamps stored/dragged widths synchronously to `[320px, max(320px, 50% workspace)]` on read and write (`SEC-U3-03`) and degrades gracefully if `localStorage` or `ResizeObserver` is unavailable.
- **Recovery guarantees:** N/A — pure Web UI shell, layout, and docking changes; no Main, Worker, Sandbox, NATS, PostgreSQL, or LLM checkpoint/recovery path is modified.

### Security (`rules/security.md` 12-Category Matrix)

| Category | Status | Mechanism (`path:line`) & Proving Test / Justification |
|---|---|---|
| Trust boundaries and identity | N/A | Pure-Web presentation/shell change; no ingress headers, session/token parsing, or backend identity paths modified. |
| Authorization (object level) | Applicable (`SEC-U3-01` hardened) | `FlowEditor.tsx:198-206` guards `handleAddNode` on `safeDisabled` (tested in `FlowEditor.test.tsx`); `EditPipeline.tsx:381` propagates `viewMode` to `ConfigurationTab.tsx:283,332-357`, disabling `EditorPanel` and withholding `"Test Pipeline"`, `ChatPanel`, and Run History in `ViewMode.Public` (tested in `ConfigurationTab.test.tsx` and `EditPipeline.test.tsx`). Publish remains hidden (`EditPipelineActions.tsx:108`). |
| Input, parsing and amplification | Applicable (`SEC-U3-02`, `SEC-U3-03` hardened) | `isEndReachable` (`pipelineReadiness.helpers.ts:44-66`) bounds traversal with a `visited` `Set`; `findFirstNodeIssue` and `collectBlockingNodeIssuesById` (`pipelineReadiness.helpers.ts:104-131`) enforce string ID checks, null-prototype maps, and `Object.hasOwn` lookups (tested in `pipelineReadiness.helpers.test.ts`); `useTestPanelWidth.ts:18-56` clamps initial and written storage widths to `[320px, maxWidth]` (tested in `useTestPanelWidth.test.ts`). |
| Injection and construction | Applicable | Zero `dangerouslySetInnerHTML`, `eval`, raw HTML/Markdown injection, or dynamic URL sinks; all tooltips, labels, and node issue messages render as plain React text nodes. |
| Egress and SSRF | N/A | No outbound server/worker/client HTTP fetch or external link targets added or modified. |
| Secrets | Applicable | `git diff --cached` secret scan = `0` hits; `useTestPanelWidth` stores only clamped numeric pixel width under `el.pipeline.testPanel.width`. |
| Supply chain and deployment | Applicable | Zero dependencies added (`package.json`/`package-lock.json` untouched); `npm audit --audit-level=high` = `0` high/critical (`21` pre-existing moderate dev-dep advisories). |
| Cross-layer differentials and version skew | Applicable (`SEC-U3-02` hardened) | `getTestBlockReason` (`pipelineReadiness.helpers.ts:137-158`) normalizes numeric YAML node identifiers via `normalizePipelineNodeIdentifiers` before evaluating `entry_point`/`nodes`, matching `parseYaml`, `collectGraphAdmissionIssues`, and the Rust Worker (tested in `pipelineReadiness.helpers.test.ts`). |
| Agentic delegation and untrusted content isolation | N/A | No delegation, HITL, MCP tool schema, or chat message/frame rendering paths altered (`PipelineTestChat.tsx`, `DelegationNotices`, `StaticPauseCard`, `NodeRecoveryNotice` untouched). |
| TOCTOU, concurrency races, and effect replay | Applicable | `useTestPanelWidth.ts:62-92` uses RAF cleanup and `wasResizingRef` edge guard; `ChatPanel.tsx:283-286` stops in-progress runs before clearing; keeping `ChatPanel` mounted while closed avoids duplicate conversation creation. |
| Sandbox escape and resource isolation | N/A | Pure-Web UI; no sandbox containers, runner envs, or workspace filesystem paths touched. |
| AuthN, OAuth, PAT, and cryptographic hygiene | N/A | No authentication, OAuth/OIDC, cookie, PAT, or cryptographic code touched. |

## 6. Fixtures & Follow-ups

- **Fixtures:** Unit and component tests use MSW handlers and Testing Library (`EditPipelineConfigurationPanel.test.tsx`, `EditPipeline.test.tsx`, `ConfigurationTab.test.tsx`, `ChatPanel.test.tsx`, `EditorPanel.test.tsx`, `FlowEditor.test.tsx`, `pipelineReadiness.helpers.test.ts`, `useTestPanelWidth.test.ts`).
- **Follow-ups:** Refresh Linux Playwright visual snapshots for the Edit Pipeline page once Wave U3 PRs land, and perform integrated real-browser verification on a disposable stack.
