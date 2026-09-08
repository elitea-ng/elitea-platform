/**
 * MCP auth constants — merged port of three old-app files (unit A5, spec
 * §9.3 A5 sourceFiles):
 *  - apps/elitea-ui/src/[fsd]/features/mcp/lib/constants/mcAuth.constants.js
 *  - apps/elitea-ui/src/[fsd]/features/mcp/lib/constants/mcpAuthFlow.constants.js
 *  - apps/elitea-ui/src/[fsd]/features/mcp/lib/constants/mcpClient.constants.js
 *
 * Merged into one file (all three were <35 lines each, thematically the
 * same "MCP auth" constant surface) — nothing here approaches the §3.5
 * 400-line budget.
 *
 * `MC_TOKENS_STORAGE_KEY`/`MCP_CREDENTIALS_STORAGE_KEY`/
 * `MCP_IGNORED_SERVERS_STORAGE_KEY` are LOGICAL keys consumed by
 * `shared/lib/storage.ts`'s `createStorage()`, which prefixes every key
 * with `el.` (spec §5.4) — so the on-disk key is `el.mcp.tokens`, not the
 * old app's raw `elitea_mcp_tokens_v1`. This is an intentional, harmless
 * rename: both the writer (this slice) and the only reader (this slice)
 * go through the same wrapper, and the old raw names are exactly the kind
 * of un-namespaced key the `el.` prefix (and its logout-sweep guarantee,
 * `clearNamespace()`) was introduced to replace — see
 * `entities/mcp/model/types.ts`'s header, which explicitly defers this
 * rename to "a later unit" (this one).
 */

export const MC_TOKENS_STORAGE_KEY = 'mcp.tokens';
export const MCP_CREDENTIALS_STORAGE_KEY = 'mcp.credentials';
export const MCP_IGNORED_SERVERS_STORAGE_KEY = 'mcp.ignoredServers';

/** Cross-window/tab notification, dispatched on `window` (not persisted). */
export const MCP_TOKEN_CHANGE_EVENT = 'elitea_mcp_token_change';

/** Marker `access_token` value for header-based auth servers that need no real OAuth token. */
export const MCP_CONNECTION_VERIFIED = '__connection_verified__';

/** Prefix for pre-built MCP toolkit types (e.g. `mcp_github`, `mcp_context7`). */
export const MCP_PREBUILD_PREFIX = 'mcp_';

export const MCP_OAUTH_ERRORS = {
  POPUP_BLOCKED: 'Popup blocked',
  AUTHORIZATION_CANCELLED: 'Authorization cancelled by user',
  NO_CODE: 'No authorization code received',
  STATE_MISMATCH: 'State mismatch',
  TOKEN_FAILED: 'Token request failed',
  MISSING_ACCESS_TOKEN: 'Token response missing access_token',
  MISSING_CLIENT_ID: 'Client ID is required',
  MISSING_ENDPOINTS: 'Authorization server metadata is missing endpoints',
  NO_AUTH_SERVERS: 'No authorization server found in MCP resource metadata',
  REGISTRATION_FAILED: 'Dynamic client registration failed',
} as const;

export const MCP_SESSION_CONFIG = {
  CHECK_INTERVAL: 500,
  POPUP_SIZE: { width: 500, height: 700 },
  SUCCESS_CLOSE_DELAY: 1500,
} as const;

/** MCP-protocol discovery well-known paths (probed against the REMOTE MCP server, not the Elitea backend). */
export const MCP_DISCOVERY_PATHS = ['/.well-known/mcp', '/.well-known/oauth-protected-resource', '/.well-known/oauth-protected-resource/mcp/'] as const;

/** OAuth-server discovery well-known paths (probed against the REMOTE MCP server). */
export const MCP_OAUTH_DISCOVERY_PATHS = ['/.well-known/oauth-authorization-server', '/.well-known/openid-configuration', '/oauth/.well-known/openid-configuration'] as const;

export const MCP_API_KEY_TEST_ENDPOINTS = ['/health', '/status', '/api/v1/health', '/api/health'] as const;

export const MCP_API_KEY_HEADERS = ['X-API-Key', 'Authorization', 'Api-Key', 'X-Access-Token'] as const;

export const MCP_AUTH_METHODS = {
  OAUTH: 'oauth',
  API_KEY: 'api_key',
  OPEN: 'open',
  NONE: 'none',
} as const;

export const MCP_DISCOVERY_TYPES = {
  MCP: 'mcp-discovery',
  OAUTH: 'oauth-discovery',
  API_KEY: 'api-key-discovery',
  OPEN: 'open-access',
  FALLBACK: 'fallback',
} as const;

export const MCP_CLIENT_DEFAULTS = {
  TIMEOUT: 30000,
  RETRIES: 3,
} as const;
