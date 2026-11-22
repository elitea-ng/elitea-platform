# Pipeline UI Wave U2 Non-Intrusive Building Blocks & Narrow-Viewport Overlap Fix (`FU-283`, 2026-11-22)

- **Branch:** `feat/fu283-u2-nonintrusive-blocks` (base `origin/main` `4f9d63873`)
- **Scope:** Pure Web (`apps/elitea-web`) + source-mapping documentation. Zero Rust, Go, Helm, Docker, or CI changes. Zero new dependencies. Zero edits to `knip.json` or top-level `src/features/pipelines/index.ts`.
- **Reference:** `EliteaUI` `origin/main` (`46d85cda7`) `flow-editor/ui/nodes/base-node/*`, `flow-editor/ui/node-panel/*`, `flow-editor/ui/canvas-controls/*`, `flow-editor/lib/helpers/nodeOperations.helpers.js`; `handoffs/point5-wave1-20261008/fu283/WAVE-U1-PLAN.md` §4.

---

## 1. Business Behavior Ported & Replatform Invariants Preserved

### 1.1 Standalone Wave U2 Building Blocks (Non-Intrusive)
Currently mounted `EditorPanel` and `NodeCard` remain untouched so all desktop (`>= 1200px`) visual regression baselines (`e2e/visual/pipeline-editor.visual.spec.ts`) stay intact until post-ETL Wave U2 wiring. Each new module exposes its own subdirectory `index.ts` barrel (automatically covered by `knip.json` L64 `"src/features/**/index.ts"` without consuming top-level `pipelines/index.ts` §3.5 export budget).

| Module | Anchor | Behavior |
|---|---|---|
| `ui/node-card-v2/` | `apps/elitea-web/src/features/pipelines/ui/node-card-v2/NodeCardV2.tsx`, `NodeCardV2Header.tsx`, `NodeCardV2Summary.tsx`, `NodeCardV2StatusIcon.tsx`, `nodeCardV2.helpers.ts`, `index.ts` | Compact 280px (`17.5rem`) view-only `NodeCardView` with header + 2-line read-only summary covering all 14 compiler-admitted node types (10 `EliteaUI` types + replatform `Map`, `Parallel`, `SplitOut`, `Aggregate`), `< 0.6` zoom mini-card mode (`isMiniCardZoom`), status icon (`running` > `error` > `needsConfiguration`), admission badge/icon slot compatible with `NodeAdmissionIssues`, normal `handles(isExpanded)` slot, and dedicated `errorOutputHandle` slot compatible with `NodeErrorOutputHandle` (`on_error: error_output`). |
| `ui/node-config-drawer/` | `apps/elitea-web/src/features/pipelines/ui/node-config-drawer/NodeConfigDrawer.tsx`, `NodeConfigPanel.tsx`, `NodeConfigPanelHeader.tsx`, `NodeConfigFooter.tsx`, `NodeConfigSection.tsx`, `NodeYamlView.tsx`, `index.ts` | 400px (`25rem`) slide-over `NodeConfigDrawer` & `NodeConfigPanel`: header with `NodeTypeIconTile`, `PipelineNodeDescriptions` tooltip, safe external docs link (`getNodeDocsLink` + `target="_blank" rel="noopener noreferrer"`), and close button; Flow / YAML `TabGroupButton` view toggle (`NodeYamlView` wrapping `YamlCodeEditor` + error alert); Basic section with `NodeFieldHeading` (`getNodeFieldInfo` + `InfoTooltip`) and `toolPicker` composition slot for Wave U1 `tool-picker`; Advanced `BasicAccordion` hosting **Q6** interrupt switches (`CommonInterruptSettings`) and `NodeSettings`; Footer with Entrypoint badge / "Make Entrypoint", **Q2** disabled `"Build with AI"` (`"Coming soon"` tooltip), Duplicate, and Delete actions. |
| `ui/canvas-controls/` + `lib/duplicateNode.ts` | `apps/elitea-web/src/features/pipelines/ui/canvas-controls/CanvasControls.tsx`, `CanvasControlButton.tsx`, `CanvasArrangeMenu.tsx`, `CanvasMiniMap.tsx`, `index.ts`, `apps/elitea-web/src/features/pipelines/lib/duplicateNode.ts` | Standalone `@xyflow/react` `CanvasControls` (zoom in/out, fit view, lock/unlock toggle, auto-arrange `TB`/`LR` menu, minimap toggle) + `CanvasMiniMap` (CSS-variable themed, drawer-offset aware) + pure `duplicateNode` helpers (`canDuplicateNodeType`, `getDuplicateNodeId`, `clearNodeConnections` stripping `transition`, `settings.error_transition`, and Router/Decision/HITL outgoing routes, `duplicateYamlNode`, `duplicateFlowNode`). |

### 1.2 Targeted `< 1200px` Narrow-Viewport Overlap Fix
Mounted layout fix strictly scoped to viewports `< 1200px` (`isSmallWindow`), leaving `>= 1200px` desktop layout and visual baselines unchanged:

- `apps/elitea-web/src/features/pipelines/ui/ConfigurationTab.tsx`: fixed `editorPanel.maxWidth` to `'100%'` (previously unconditionally `calc(100% - 21.5625rem)` even when stacked vertically), added `minHeight: '32rem'` and `flex: '0 0 auto'` when `isSmallWindow`, and switched `splitContainer` to `flex: '0 0 auto'`, `height: 'auto'`, `width: '100%'` when `isSmallWindow`.
- `apps/elitea-web/src/features/pipelines/ui/GeneralFormPanel.tsx`: changed `flex` to `isSmallWindow ? '0 0 auto' : 3` so `overflowY: 'visible'` content never shrinks and overlaps `splitContainer` in column layout.
- `apps/elitea-web/src/features/pipelines/ui/ChatPanel.tsx`: switched `chatContainerSx` (`height: 'auto'`, `flex: '0 0 auto'`), `chatContentSx` (`height: 'auto'`, `overflowY: 'visible'`), and `topBarSx` (`paddingLeft: '0'`) when `isSmallWindow`.
- `apps/elitea-web/src/widgets/app-shell/ui/ShellTopBar.tsx` & `apps/elitea-web/src/pages/pipelines/EditPipeline.tsx`: added `minWidth: 0` and `overflow: 'hidden'` on breadcrumb bar/slot/title (`textOverflow: 'ellipsis'`, `whiteSpace: 'nowrap'`) and `flexWrap: 'wrap', minWidth: 0` on `EditPipeline` toolbar actions so long pipeline titles truncate cleanly and never overlap controls below 1200px.

---

## 2. Verification & Gate Results

- **Targeted & Full Pipelines Vitest (`apps/elitea-web`, Node 24):**
  - New Wave U2 unit tests (`NodeCardV2.test.tsx`, `NodeConfigDrawer.test.tsx`, `CanvasControls.test.tsx`, `duplicateNode.test.ts`): **4 files, 16/16 passed**.
  - Narrow-viewport responsive layout tests (`ConfigurationTab.test.tsx`, `GeneralFormPanel.test.tsx`, `ChatPanel.test.tsx`, `ShellTopBar.truncation.test.tsx`): **4 files, 38/38 passed**.
  - Full `src/features/pipelines` + `ShellTopBar.truncation.test.tsx` suite: **265/265 files, 2,908 passed, 1 pre-existing expected fail, 0 failures**.
- **Static & Policy Gates (`apps/elitea-web`, Node 24):**
  - `npm run typecheck`: PASS (`0` errors)
  - `npm run lint -- --deny-warnings`: PASS (`0` warnings, `0` errors)
  - `npm run theme-gate`: PASS (all 9 checks + 5/5 brand-pack contract tests)
  - `node scripts/check-budgets.mjs`: PASS (`7380` files within §3.5 budgets)
  - `node scripts/check-testid-namespace.mjs`: PASS (`2892` test IDs in kebab-case namespace)
  - `node scripts/check-layer-cycle.mjs`: PASS (`5904` modules, `0` errors/warnings)
  - `node scripts/check-dead-code.mjs`: PASS (`0` knip issues)
  - `node scripts/i18n-backfill.mjs --check`: PASS (`31` new keys added to `src/shared/i18n/en.json`)
  - `npm run check-handlers`: PASS
- **Visual Regression & Real-Browser Proof:**
  - `e2e/visual/pipeline-editor.visual.spec.ts` desktop (`1602x848` and `1280x800`, both `>= 1200px`) styles are byte-for-byte unchanged (`isSmallWindow === false`). Real-browser Playwright visual run deferred to Wave U2 integration / CI stack.

---

## 3. Performance / Durability / Resilience / Security

- **Performance:** All new components are `React.memo`-wrapped with pure helper derivations (`getNodeCardV2Summary`, `duplicateNode`); mini-card zoom threshold (`< 0.6`) drops summary DOM subtrees when zoomed out.
- **Durability / Resilience:** N/A (pure Web UI presentation & pure in-memory YAML/Flow node cloning; no backend persistence or runtime state mutations). `duplicateNode` preserves 5b-1 resilience invariants by stripping `settings.error_transition` on cloned nodes while keeping other settings intact.
- **Security:**
  - External documentation links in `NodeConfigPanelHeader` use `NODE_DOCS_LINK_REL` (`rel="noopener noreferrer"`) with `target="_blank"`, sourcing URLs solely from static `NODE_DOCS_URLS` / `DeprecatedTips` constants (no user-supplied `href` injection).
  - Tooltip and summary text render as React text children (no `dangerouslySetInnerHTML`).
  - Zero secrets in diff (`git diff` secret-scanned: `0` hits); zero new dependencies.
