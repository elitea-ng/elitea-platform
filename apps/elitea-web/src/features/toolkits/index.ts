/**
 * Public API — spec §3.3: named exports only, curated (§3.5 budget: ≤20
 * exported symbols, shared across every A4x sub-unit that owns files under
 * `features/toolkits` — some land concurrently in this same worktree, some
 * do not yet exist. This is sub-unit A4e's ("Toolkits list/tab-bar UI +
 * SharePoint OAuth sub-tree") first-landing contribution to this barrel:
 * only the three components a `pages/`/`widgets/`-layer caller genuinely
 * needs a cross-slice entry point for.
 *
 * `ToolkitsList` is exported per this batch's own note ("list/tab-bar
 * pieces are consumed by A4g's pages"). `ToolkitsTabBar`/`ToolkitsControls`
 * are exported for the same reason — a future `pages/toolkits` (or a
 * `widgets/`-layer toolkit-edit composition) assembles them alongside
 * `entities/toolkit`'s `ToolConfigurationForm` and this domain's own
 * `ToolkitEditor` (a different A4 sub-unit's owned file, per the batch
 * brief's cross-domain "MUST EXPORT" list).
 *
 * The SharePoint sub-tree (`sharepoint/**`) is deliberately NOT exported
 * here — per the batch brief, its real consumer (`ui/form/ToolBase/
 * ToolBase.tsx`, A4c's owned file) lives INSIDE this same `features/
 * toolkits/` slice, so it reaches `sharepoint/**` via an ordinary
 * intra-slice import (R-L3: intra-slice imports are unrestricted) with no
 * need to spend any of this budget. Promote a `Sharepoint*` symbol here the
 * moment a genuine cross-slice (`pages/`/`widgets/`) consumer needs one
 * directly.
 *
 * `ToolkitTypesPanel`/`ToolkitsEmptyListPlaceHolder`/`ToolkitsEmptyState`/
 * `ToolkitsTabBarPlaceholder` stay intra-slice-only too — each is already
 * consumed by `ToolkitsList`/`ToolkitsTabBar` internally, and no evidence
 * (baseline call sites or this batch's brief) points to a caller needing
 * one of them standalone.
 */
export { ToolkitsList } from './ui/list/ToolkitsList';
export type { ToolkitListItem } from './ui/list/ToolkitsList';
/**
 * `ToolkitsListState`/`ToolkitsListTypeFilter` (the two grouped-prop types
 * `ToolkitsListProps.listState`/`.typeFilter` reference — added when this
 * component's own prop count needed grouping to fit the §3.5 `component-
 * props` budget) are deliberately NOT spent here: a caller builds its
 * `listState`/`typeFilter` object literal inline and TypeScript checks it
 * structurally against `ToolkitsListProps['listState']`/`['typeFilter']`
 * with no import needed, same "caller assembling the deps object types it
 * structurally... without needing a separate import" precedent
 * `features/agents/index.ts`'s own doc comment already established for
 * `AgentEditorDeps`.
 */

export { ToolkitsTabBar } from './ui/toolkits-tab-bar/ToolkitsTabBar';

export { ToolkitsControls } from './ui/toolkits-tab-bar/ToolkitsControls';

/**
 * Unit A4g's own landing (`pages/NewChat/ToolkitEditor.jsx` port + the
 * create/edit/delete/export toolkit-management surface). `ToolkitEditor`
 * MUST be exported here — Wave-2 unit C6 needs it cross-feature (this
 * batch's own mission brief).
 *
 * Budget note: 11 slots already spent by the block above (A4e, as landed at
 * the time this block was written — re-check before adding more). Only
 * `ToolkitEditor` (+ the 4 types a `deps`-wiring caller needs) plus
 * `DeleteToolkitButton`/`ExportToolkitButton` (standalone, page-composable,
 * no ambient dependency on the rest of this unit's internals) are exported
 * — 7 more, 18/20 total. `CreateToolkitButton`/`SaveToolkitButton` stay
 * intra-slice-only: their only real callers today are `ToolkitEditor`
 * (intra-slice, R-L3-legal) and `pages/toolkits/CreateToolkit.tsx` (below),
 * which reaches the create flow through `CreateToolkitToolTabBar`'s own
 * `onSave` callback instead (matching the baseline's own choice of tab-bar
 * over standalone button for that specific page — see that page's own doc
 * comment), so neither standalone button needs a cross-slice entry point
 * yet.
 */
export { ToolkitEditor } from './ui/ToolkitEditor';
export type { ToolkitEditorDeps, ToolkitEditorParticipant } from './ui/ToolkitEditor';
export { DeleteToolkitButton } from './ui/DeleteToolkitButton';
export { ExportToolkitButton } from './ui/ExportToolkitButton';

/**
 * Unit C6 additions (`useEditToolkit`/`useToolkitCreation`): fully ported,
 * intra-slice-only today, needed cross-feature by C6's `deps`-composition
 * root. Bundled into one `toolkitEditorHooks` object export (the established
 * convention) to consume exactly the 1 remaining free slot (19/20 → 20/20).
 */
export { toolkitEditorHooks } from './model/toolkitEditorHooks';
export { ToolkitForm } from './ui/form/ToolkitForm/ToolkitForm';
export { ToolkitTypeSelector } from './ui/ToolkitTypeSelector';
export { CreateToolkitToolTabBar } from './ui/CreateToolkitToolTabBar';
export { ConfigurationTab } from './ui/ConfigurationTab';

/**
 * Phase 1c: the real create/edit mutations. These replace the injected
 * `deps.createToolkit`/`deps.saveToolkit` stubs that existed only because the
 * operations were missing from the OpenAPI spec — the handlers were always
 * mounted. See the CORRECTION in `api/toolkits.ts`.
 */
export { useToolkitCreate, useToolkitEdit } from './api/toolkits';

/**
 * Issue #149 — the Indexes tab's mount point. `IndexesTab` is the ONE
 * component `pages/toolkits/EditToolkit.tsx` renders for that tab (see its
 * own module doc for why the eight intra-slice dependencies it binds cannot
 * be spent here individually), and `useIndexesTabVisibility` is the ONE hook
 * that decides whether the tab is offered at all — the baseline's
 * `shouldHideIndexesTab`, which this port had dropped. Two symbols, not ten.
 */
export { IndexesTab } from './ui/IndexesTab';
export type { IndexesTabChatUI } from './ui/IndexesTab';
export { useIndexesTabVisibility } from './lib/hooks/useIndexesTabVisibility';

/*
 * Budget note (§3.5, ≤20). An earlier round dropped `ToolkitsEmptyStateConfig`
 * and `ToolkitsTabBarProps` for the same reason this one drops four more:
 * they had ZERO consumers anywhere in `src/`. The #149 block above adds three
 * symbols, so `ToolkitsListProps`, `ToolkitTypeTag`, `ToolkitsControlsProps`
 * and `ToolkitEditorProps` come off — each verified (grep across all of
 * `src/`, excluding this slice) to have no importer outside
 * `features/toolkits` at all; `ToolkitEditorProps`' only non-slice mention is
 * inside this slice's own test. All four remain exported from their own
 * modules for in-slice use, so nothing is deleted, only de-published.
 * Back to 20/20 — re-check before adding more.
 */

export type { ToolkitTestAuthorizationRenderProps } from './ui/test-tools/TestToolPane';
