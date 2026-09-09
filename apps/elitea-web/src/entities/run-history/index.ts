/**
 * Public API — spec §3.3: named exports only.
 *
 * Issue #868's run-history panel: one component shared by the agents',
 * pipelines' and toolkits' editors, each of which wires it into its own
 * `renderRunHistory`-shaped slot.
 */
export { RunHistoryPanel } from './ui/RunHistoryPanel';
export type { RunHistoryEntityName, RunHistoryPanelProps } from './ui/RunHistoryPanel';
