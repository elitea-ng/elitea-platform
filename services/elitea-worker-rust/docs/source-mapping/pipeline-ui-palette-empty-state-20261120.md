# Pipeline UI Parity — Wave U1: Add-Node Palette, Empty Canvas State, Node Field Info & Docs Constants (FU-283)

- **Date:** 2026-11-20
- **Branch:** `feat/fu283-u1-palette-empty-state` (based on `origin/main` `5f21740a8`)
- **Reference UI:** `EliteaUI` `origin/main` (`46d85cda7`)
- **Scope:** Pure-Web (`apps/elitea-web`) Wave U1 track `ui-u1-palette` (Gaps #3 / Q9, #4 / Q7, #17 in `WAVE-U1-PLAN.md`)

---

## 1. Business Behavior Ported vs. Not Ported

### Ported from `EliteaUI` (`46d85cda7`)
- **Add-node Palette (Gap #3 / Q9):**
  - Ported `NodeTypeSelector`, `NodeTypeCard`, `NodeTypeIconTile`, `AddNodeButton`, and `nodeTypeSelector.helpers` from `src/[fsd]/features/pipelines/flow-editor/ui/add-node/*` and `lib/helpers/nodeTypeSelector.helpers.js`.
  - Flat alphabetical 1–2 column responsive `"Select node type"` card grid (icon tile + display name + 2-line clamped description, no categories), fitted inside the canvas boundary with layout stability during popover exit.
  - Toolbar `"Node"` button (`BaseBtn` `variant="iconLabel"` + `PlusIcon`) with `aria-label="Add node"` (WCAG 2.5.3 compliant) and `#pipeline-add-node-menu` / `role="menu"` / `role="menuitem"` semantics preserved for existing E2E and integration tests.
  - Verbatim descriptions for the 10 `EliteaUI` node types (`Agent`, `Code`, `Decision`, `Hitl`, `LLM`, `Mcp`, `Printer`, `Router`, `StateModifier`, `Toolkit`) plus concise matching descriptions for the 4 replatform-admitted node types (`Aggregate`, `Map`, `Parallel`, `SplitOut`), gated strictly by `RuntimeContractConstants.isCompilerAdmittedNodeType`.
- **Empty Canvas State & Hidden `End` Node (Gap #4 / Q7):**
  - Ported `FlowEmptyState` from `src/[fsd]/features/pipelines/flow-editor/ui/empty-state/FlowEmptyState.jsx` (`"Start by adding your first node to the canvas!"`, theme-aware light/dark illustration reusing `src/assets/empty-states/applications-light.webp` and `applications-dark.webp`, `"Add Node"` CTA opening the centered `NodeTypeSelector`).
  - Wired into `FlowEditor.tsx`: while `yamlJsonObject.nodes` has 0 user nodes, `FlowEmptyState` renders over the canvas and the `End` node is filtered from `<ReactFlow nodes={visibleFlowNodes} />` while staying in `flowNodes` state for persistence/layout.
- **Node Field Info & Node Docs Constants (Gap #17):**
  - Ported `nodeDocs.constants.js` + `nodeDocs.helpers.js` into `nodeDocs.constants.ts` (static `https://docs.elitea.ai/how-tos/pipelines/nodes/...` URLs, `MIGRATION_GUIDE_TOOLTIP`, `NODE_DOCS_LINK_REL = 'noopener noreferrer'`, `getNodeDocsLink`).
  - Ported `nodeFieldInfo.constants.js` + `nodeFieldInfo.helpers.js` into `nodeFieldInfo.constants.ts`, extended with replatform node types (`Aggregate`, `Map`, `Parallel`, `SplitOut`) and per-node resilience `settings` keys (`on_error`, `always_output_data`, `execute_once`, `retry_on_fail`, `max_tries`, `wait_between_tries_ms`, `timeout_ms`) ready for Wave U2's Node Panel.

### Deliberately Not Ported / Preserved Replatform Behavior
- **Deprecated node authoring:** `Condition`, `Custom`, `Function`, `Loop`, `LoopFromTool`, `Pipeline`, and `Tool` remain excluded from the palette (`getAddableNodeTypes` intersects with `RuntimeContractConstants.CompilerAdmittedNodeTypes`).
- **Interactive tours & GA tracking:** `data-tour` attributes and `trackEvent` calls from `EliteaUI` are omitted (no tour/GA domain in `apps/elitea-web`).
- **`CreatePipeline.tsx` / `PIPELINE_STARTER_TEMPLATE` left intact:** In `apps/elitea-web`, `CreatePipeline.tsx` currently renders a form-only page (`CreateAgentForm` + `CreatePipelineAdmissionAlert`), not `FlowEditor` / `PipelineEditorWorkspace` (which lands in Wave U2). Removing `PIPELINE_STARTER_TEMPLATE` before Wave U2 would break `CreatePipeline.test.tsx` and produce unrunnable blank pipelines on Save. Empty-canvas Create parity will align when `PipelineEditorWorkspace` replaces the form-only Create page in Wave U2.
- **Kebab-case `data-testid` normalization:** `toNodeTypeTestIdSegment` converts underscores to hyphens (`state_modifier` -> `state-modifier`, `split_out` -> `split-out`) to satisfy `check-testid-namespace.mjs`.

---

## 2. Changed Source Paths

- `apps/elitea-web/src/features/pipelines/lib/flow-editor/constants/nodeDocs.constants.ts` (L1-L45) — `NODE_DOCS_URLS`, `NODE_DOCS_LINK_REL`, `MIGRATION_GUIDE_TOOLTIP`, `getNodeDocsLink`
- `apps/elitea-web/src/features/pipelines/lib/flow-editor/constants/nodeFieldInfo.constants.ts` (L1-L282) — `NODE_FIELD_KEYS`, `COMMON_NODE_FIELD_INFO`, `NODE_FIELD_INFO` (10 `EliteaUI` + 4 replatform + 7 resilience settings + deprecated types), `getNodeFieldInfo`, `getNodeFieldInfoOf`, `getInputMappingFieldInfo`, `getValueDataTypeInfo`
- `apps/elitea-web/src/features/pipelines/lib/flow-editor/constants/index.ts` (L28-L47) — re-exports `NodeDocsConstants`, `NodeFieldInfoConstants`
- `apps/elitea-web/src/features/pipelines/ui/add-node/nodeTypeDescriptions.ts` (L1-L26) — 14 admitted node descriptions
- `apps/elitea-web/src/features/pipelines/ui/add-node/nodeTypeSelector.helpers.ts` (L1-L79) — `getNodeTypeSelectorLayout`, `getAddableNodeTypes`, `toNodeTypeTestIdSegment`
- `apps/elitea-web/src/features/pipelines/ui/add-node/NodeTypeIconTile.tsx` (L1-L40) — 28px rounded color icon tile
- `apps/elitea-web/src/features/pipelines/ui/add-node/NodeTypeCard.tsx` (L1-L94) — accessible `menuitem` card with icon tile, display name, and 2-line clamped description
- `apps/elitea-web/src/features/pipelines/ui/add-node/NodeTypeSelector.tsx` (L1-L148) — responsive 1–2 col Popover grid with `#pipeline-add-node-menu`, `role="menu"`, and exit-transition layout stability
- `apps/elitea-web/src/features/pipelines/ui/add-node/AddNodeButton.tsx` (L1-L52) — `"Node"` toolbar trigger button (`BaseBtn` `iconLabel`)
- `apps/elitea-web/src/features/pipelines/ui/add-node/index.ts` (L1-L16) — barrel export
- `apps/elitea-web/src/features/pipelines/ui/AddNodeMenu.tsx` (L1-L29) — preserves `AddNodeMenu` wrapper over `AddNodeButton`
- `apps/elitea-web/src/features/pipelines/ui/empty-state/FlowEmptyState.tsx` (L1-L110) — theme-aware empty-canvas overlay + centered `NodeTypeSelector`
- `apps/elitea-web/src/features/pipelines/ui/empty-state/index.ts` (L1) — barrel export
- `apps/elitea-web/src/features/pipelines/ui/FlowEditor.tsx` (L1-L361) — `isEmptyPipelineDocument`, `getVisibleFlowNodes` (hiding `End` while empty), renders `FlowEmptyState`
- `apps/elitea-web/src/shared/i18n/en.json` — backfilled 4 new keys (`features.pipelines.addNode.buttonLabel`, `features.pipelines.addNode.selectNodeType`, `features.pipelines.emptyState.addNode`, `features.pipelines.emptyState.message`)
- Tests:
  - `apps/elitea-web/src/features/pipelines/lib/flow-editor/constants/nodeDocs.constants.test.ts` (27 tests)
  - `apps/elitea-web/src/features/pipelines/lib/flow-editor/constants/nodeFieldInfo.constants.test.ts` (13 tests)
  - `apps/elitea-web/src/features/pipelines/ui/add-node/nodeTypeSelector.helpers.test.ts` (8 tests)
  - `apps/elitea-web/src/features/pipelines/ui/add-node/NodeTypeSelector.test.tsx` (11 tests)
  - `apps/elitea-web/src/features/pipelines/ui/empty-state/FlowEmptyState.test.tsx` (3 tests)
  - `apps/elitea-web/src/features/pipelines/ui/AddNodeMenu.test.tsx` (19 tests)
  - `apps/elitea-web/src/features/pipelines/ui/FlowEditor.test.tsx` (18 tests, +3 empty-state/hidden-End tests)
  - `apps/elitea-web/src/features/pipelines/ui/GraphExtensionsAdmission.test.tsx` (3 tests, updated `aria-label` lookup)
  - `apps/elitea-web/src/features/pipelines/ui/ParallelAuthoringGate.test.tsx` (4 tests, updated `aria-label` lookup)

---

## 3. Verification & Test Counts

- **Targeted & pipeline vitest suites:**
  - `nodeDocs.constants.test.ts`: 27 passed, 0 skipped
  - `nodeFieldInfo.constants.test.ts`: 13 passed, 0 skipped
  - `nodeTypeSelector.helpers.test.ts`: 8 passed, 0 skipped
  - `NodeTypeSelector.test.tsx`: 11 passed, 0 skipped
  - `FlowEmptyState.test.tsx`: 3 passed, 0 skipped
  - `AddNodeMenu.test.tsx`: 19 passed, 0 skipped
  - `AddNodeMenuYaml.integration.test.tsx`: 6 passed, 0 skipped
  - `GraphExtensionsAdmission.test.tsx`: 3 passed, 0 skipped
  - `ParallelAuthoringGate.test.tsx`: 4 passed, 0 skipped
  - `FlowEditor.test.tsx`: 18 passed, 0 skipped
  - `EditorPanel.test.tsx`: 21 passed, 0 skipped
  - `lib/flow-editor/constants/*` (10 files) + `CreatePipeline.test.tsx` (1 file): 167 passed, 0 skipped
- **Static gates (`apps/elitea-web`, Node 24):**
  - `npm run typecheck`: PASS (0 errors)
  - `npm run lint -- --deny-warnings`: PASS (0 warnings, 0 errors on 6,512 files)
  - `npm run theme-gate`: PASS (all 9 checks + 5/5 brand-pack contract tests)
  - `node scripts/check-budgets.mjs`: PASS (7,291 files within §3.5 budgets)
  - `node scripts/check-testid-namespace.mjs`: PASS (2,801 test ids)
  - `node scripts/check-layer-cycle.mjs`: PASS (0 errors, 0 warnings)
  - `node scripts/check-dead-code.mjs`: PASS (Knip: 0 issues)
  - `node scripts/i18n-backfill.mjs --check`: PASS
- **Supply chain (`npm audit`):** Zero dependencies added or modified. `npm audit` could not reach `registry.npmjs.org` inside the offline sandbox (`ENOTFOUND registry.npmjs.org`); recorded as open/offline-skipped rather than a pass.
- **Secret scan:** `/usr/bin/grep` on `git diff` found 0 secret/token matches.

---

## 4. Performance, Durability, Resilience, Security & Recovery Rows

- **Performance:** Pure client-side React presentation & memoized layout math (`nodeTypeSelector.helpers.ts:39-57`, `FlowEditor.tsx:251-255`). Zero network round trips, zero DB writes, O(14) node palette render. Proven by `nodeTypeSelector.helpers.test.ts` and `NodeTypeSelector.test.tsx`.
- **Durability:** Not applicable — no persisted state, storage schema, or backend execution path is modified; `End` node stays in `flowNodes` state (`FlowEditor.tsx:113-115`) so persisted layout semantics are unchanged.
- **Resilience:** Bounded layout dimensions (`MIN_HEIGHT = 200`, `CARD_WIDTH = 258` in `nodeTypeSelector.helpers.ts:5-11`), strict compiler allow-list filtering (`getAddableNodeTypes` in `nodeTypeSelector.helpers.ts:63-73`), and graceful empty-string fallbacks in `getNodeFieldInfo` (`nodeFieldInfo.constants.ts:261-264`). Proven by `nodeTypeSelector.helpers.test.ts`, `AddNodeMenu.test.tsx`, and `nodeFieldInfo.constants.test.ts`.
- **Security:**
  - *Trust boundaries & identity / Authorization / Egress / Secrets / Injection (SQL/shell/templates):* Not applicable (pure static React UI & constants, no routes/RPCs/network calls/secrets).
  - *Input handling & XSS / External links:* All documentation URLs in `nodeDocs.constants.ts:13-25` are static `https://docs.elitea.ai/how-tos/pipelines/nodes/...` constants (no user-controlled URLs) and expose `NODE_DOCS_LINK_REL = 'noopener noreferrer'` (`nodeDocs.constants.ts:11`); all tooltip/field info texts are plain strings rendered via React text escaping (no `dangerouslySetInnerHTML`). Proven by `nodeDocs.constants.test.ts:10-14`.
  - *Supply chain:* Zero new dependencies (`package.json` / `package-lock.json` untouched).
- **Recovery guarantee rows (Web/browser × authoring):**
  - `Web/browser × Canvas authoring (empty state / add node)` -> **I (Idempotent)** — UI-only overlay and popover state (`FlowEmptyState.tsx:59`, `AddNodeButton.tsx:18`); reloading re-derives empty state deterministically from persisted `yamlJsonObject.nodes` (`FlowEditor.tsx:108-111`). Proven by `FlowEditor.test.tsx:246-278`.

---

## 5. Real-Browser Evidence & Follow-Ups

- **Real-browser Chrome proof:** Deferred per Wave U1 plan (`WAVE-U1-PLAN.md` §3.5 — Wave U1 runs pure-Web parallel tracks with zero Rust/Docker builds or stack spin-ups; real-browser verification runs during Wave U1 integration / Chrome DevTools pass).
- **Fixtures:** Unit tests only (in-memory jsdom + `renderWithTheme`, no DB fixtures).
- **Follow-ups:**
  1. Wire `NodeFieldInfoConstants` and `NodeDocsConstants` into Wave U2's `NodeConfigPanel` / `NodeConfigHeader`.
  2. Align `CreatePipeline.tsx` blank-canvas startup when `PipelineEditorWorkspace` lands in Wave U2.
  3. Run real-browser Chrome visual + interaction confirmation (light & dark mode, narrow & wide canvas, empty canvas -> Add Node -> `End` reveal -> reload) during Wave U1 integration.
