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
export type { McpLoginAuthConfig } from './model/useMcpLogin';
export { useMcpTokenChange } from './model/useMcpTokenChange';

/**
 * The "Load Tools" fetch for a Remote or pre-built MCP toolkit.
 *
 * It is published now because `pages/toolkits` finally composes it. The
 * toolkit form has rendered a "Load Tools" action since the port, behind
 * `ToolBaseSlots.toolActionsExtra` — a caller-injected slot precisely because
 * `features/toolkits` may not import this slice (R-L1). No caller ever filled
 * it, so the action was permanently disabled and MCP tools could not be loaded
 * from the browser at all. `pages/toolkits/lib/useMcpLoadTools.tsx` is the
 * composition root that fills it, and this is the hook it needs.
 *
 * BUDGET (§3.5, ≤20 counting types). `UseMcpLoginOptions` and
 * `UseMcpLoginResult` come off to pay for it. Both had ZERO importers outside
 * this slice — verified by grep across all of `src/` — and both stay exported
 * from `model/useMcpLogin.ts` for in-slice use, so nothing is deleted, only
 * de-published. This is the same trade `features/toolkits/index.ts` records in
 * its own budget note. Back to 20/20 — re-check before adding more.
 *
 * NOT IN `shared/api/endpoints.manifest.json`, and that is deliberate. The
 * route this hook calls — `POST /elitea_core/mcp_sync_tools/prompt_lib/{id}` —
 * is not described in `api/openapi/v2.yaml`, and elitea-main's
 * `TestSpecRouterConformance/manifest_reverse_check` fails any manifest entry
 * the spec does not cover. Its allowlist may only shrink, so the entry cannot
 * be added until the operation is described. Describing it in v2.yaml, then
 * recording the endpoint, is the follow-up.
 */
export { useGetRemoteMcpTools } from './model/useGetRemoteMcpTools';

// lib — the storage/flow primitives a non-UI caller (e.g. a chat tool-action
// handler reacting to `mcp_authorization_required`, per manifest JRNY-018)
// needs without pulling in React.
export { getAccessToken, isPrebuildMcpType } from './lib/storage';
export { startMcpAuthFlow } from './lib/oauthFlow';
