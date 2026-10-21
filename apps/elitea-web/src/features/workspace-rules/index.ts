/**
 * Public API — the desktop-only approval rules panel (#1216): a workspace's
 * and the person's command prefix rules, the repository's proposal and
 * "Test a command". Reached only from `pages/workspace`, which is itself
 * reached only in the `desktop` build.
 */
export { RulesPanel } from './ui/RulesPanel';
