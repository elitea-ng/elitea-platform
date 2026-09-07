/**
 * Public API — spec §3.3: named exports only, curated (§3.5 budget: ≤20).
 *
 * The widget starts a run and renders the panel; nothing else is public.
 * `statusForSource` and `sourceStatusView` are the two pieces of decidable
 * logic in the slice and they stay internal — their tests live here, and a
 * second caller matching sources by its own rule is how two screens come to
 * disagree about whether a source has ever been ingested.
 */
export { useInventorySources, type InventorySourcesView } from './model/useInventorySources';
export { useIngestionRun, type IngestionRun } from './model/useIngestionRun';
export { InventorySourcesPanel } from './ui/InventorySourcesPanel';
export { AddSourceDialog, type SourceCandidate } from './ui/AddSourceDialog';
export { SOURCE_TOOLKIT_TYPES, useSaveSources } from './api/inventorySourcesApi';
