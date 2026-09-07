/**
 * Public API — spec §3.3: named exports only, curated (§3.5 budget: ≤20
 * exported symbols, counted by `scripts/lib/budgets-core.mjs`'s
 * `countExports`, which counts type exports too). Consumed by
 * `pages/mcps/**` and, per the layer model, any `pages/`/`widgets/`
 * component ABOVE this slice that needs to embed MCP login/logout/status
 * UI (e.g. a future toolkit-settings form in `pages/toolkits`) — never by
 * a sideways `features/` import (R-L1).
 *
 * Deliberately NOT re-exported here (available inside the slice, not part
 * of the curated surface, to stay under budget): `useMcpAuthModal`/
 * `useConfigOAuthModal` (lower-level than `useMcpLogin`, which every
 * current UI export already composes), `McpAuthStatusBadgeValues`/
 * `McpTokenChangeOptions`/`McpTokenChangeResult` (structurally inferable
 * by callers from the functions that use them), `mcpLogout` (available as
 * `getAccessToken`'s sibling in `lib/storage.ts`, one layer down, if a
 * future caller needs it enough to justify a budget trade-off).
 */

// UI — the components other layers actually mount.
export { McpAuthModal } from './ui/McpAuthModal';
export type { McpAuthModalProps } from './ui/McpAuthModal';
export { McpLogoutModal } from './ui/McpLogoutModal';
export type { McpLogoutModalProps } from './ui/McpLogoutModal';
export { McpAuthStatusBadge } from './ui/McpAuthStatusBadge';
export type { McpAuthStatusBadgeProps } from './ui/McpAuthStatusBadge';
export { McpLogInButton } from './ui/McpLogInButton';
export type { McpLogInButtonProps } from './ui/McpLogInButton';
export { McpLogInLink } from './ui/McpLogInLink';
export type { McpLogInLinkProps } from './ui/McpLogInLink';
export { McpLogoutButton } from './ui/McpLogoutButton';
export type { McpLogoutButtonProps } from './ui/McpLogoutButton';

// model — the one hook a caller composing its own MCP-login affordance needs.
export { useMcpLogin } from './model/useMcpLogin';
export type { UseMcpLoginOptions, McpLoginAuthConfig } from './model/useMcpLogin';
export { useMcpTokenChange } from './model/useMcpTokenChange';

// lib — the storage/flow primitives a non-UI caller (e.g. a chat tool-action
// handler reacting to `mcp_authorization_required`, per manifest JRNY-018)
// needs without pulling in React.
export { getAccessToken } from './lib/storage';
export { getAllTokens } from './lib/tokenRefresh';
export { getExecutionTokens } from './lib/executionTokens';
export { startMcpAuthFlow } from './lib/oauthFlow';
