/**
 * Public API — the desktop-only local MCP servers (#1230): the settings
 * panel (Settings › MCP servers) and the repository proposal card in the
 * session. Reached only from `pages/workspace`, which is itself reached
 * only in the `desktop` build.
 */
export { McpPanel } from './ui/McpPanel';
export { McpProposals } from './ui/McpProposals';
