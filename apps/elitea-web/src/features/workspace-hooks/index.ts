/**
 * Public API — the desktop-only hooks (#1237): the settings panel
 * (Settings › Hooks) and the repository proposal card in the session.
 * Reached only from `pages/workspace`, which is itself reached only in the
 * `desktop` build.
 */
export { HooksPanel } from './ui/HooksPanel';
export { HooksProposals } from './ui/HooksProposals';
