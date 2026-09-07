/**
 * Public API — spec §3.3: named exports only, curated (§3.5 budget: ≤20).
 *
 * One symbol. `IngestionBanner` is internal: it reports on a run this widget
 * owns, and a second mount of it elsewhere would describe a run that screen
 * did not start.
 */
export { InventoryWorkspace, type InventoryWorkspaceProps } from './ui/InventoryWorkspace';
