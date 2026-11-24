import type { PersonalAccessToken } from './types';

const DAY_IN_MILLISECONDS = 24 * 3600 * 1000;

/**
 * apps/elitea-ui/src/common/utils.jsx:691-705 `calculateExpiryInDays`,
 * ported verbatim (`now` is an injected parameter in place of
 * `new Date().getTime()`, to keep this pure): `null` -> `-1` ("never");
 * more than a day left -> rounded whole days; less than a day but still
 * positive -> `1`; non-positive -> `0` ("expired").
 */
export function tokenExpiryInDays(expires: string | null, now: number): number {
  if (expires === null) return -1;
  const duration = new Date(expires).getTime() - now;
  if (duration > DAY_IN_MILLISECONDS) return Math.round(duration / DAY_IN_MILLISECONDS);
  if (duration > 0) return 1;
  return 0;
}

export type TokenExpiryStatus = 'safe' | 'warning' | 'never' | 'expired';

/**
 * apps/elitea-ui/src/[fsd]/features/settings/ui/personal-tokes/
 * TokensTable.jsx:40-119 `ExpiryInDays` — the threshold/status branches
 * extracted as a pure function: `>7` days -> safe; `1-7` days -> warning;
 * `-1` (never) -> never; else (`0`) -> expired.
 */
export function tokenExpiryStatus(expires: string | null, now: number): TokenExpiryStatus {
  const days = tokenExpiryInDays(expires, now);
  if (days > 7) return 'safe';
  if (days > 0) return 'warning';
  if (days === -1) return 'never';
  return 'expired';
}

/**
 * apps/elitea-ui/src/[fsd]/features/settings/ui/personal-tokes/
 * TokensTable.jsx:278-286 — `'...' + row.token.substring(row.token.length - 4)`.
 */
export function maskedTokenValue(token: PersonalAccessToken): string {
  return '...' + token.token.slice(-4);
}

/** Alphabetical name sort — the table's default sort field/direction (`TokensTable.jsx:227-232`). */
export function sortTokensByName(tokens: readonly PersonalAccessToken[]): PersonalAccessToken[] {
  return [...tokens].sort((a, b) => a.name.toLowerCase().localeCompare(b.name.toLowerCase()));
}

/* ── project binding ───────────────────────────────────────────────────── */

/**
 * The bound project as a string key, or `null` when the token is unbound.
 * `undefined` (a record from a backend that predates the binding) and `null`
 * (an explicitly unbound record) collapse to the same answer on purpose —
 * see `PersonalAccessToken.project_id`.
 */
export function tokenProjectKey(token: PersonalAccessToken): string | null {
  const projectId = token.project_id;
  if (projectId === null || projectId === undefined) return null;
  return String(projectId);
}

/**
 * The `project_id` a create-token request may carry, or `undefined` for an
 * unbound token.
 *
 * The app holds the selected project id as a STRING; `POST /auth/token/` takes
 * a positive integer. A value of another shape (an empty selection, `'0'`, a
 * decimal, a name) gives `undefined`, and the caller then OMITS `project_id`.
 * Absent is the unbound case (`spec-llm-project-scope` §4) — a caller must
 * never send `null` or `0` in its place.
 *
 * Deep-imported by the create-token route, not re-exported from `index.ts`:
 * see that file's note on the §3.3 budget.
 */
export function bindableProjectId(selectedProjectId: string): number | undefined {
  if (!/^\d+$/.test(selectedProjectId)) return undefined;
  const id = Number(selectedProjectId);
  return Number.isSafeInteger(id) && id > 0 ? id : undefined;
}

/**
 * The two failures `POST /api/v2/auth/token/` answers for a bad `project_id`
 * (`spec-llm-project-scope` §4): 403 for a project the caller is not a member
 * of, 400 for a malformed one.
 */
export type TokenProjectErrorCode = 'project_forbidden' | 'invalid_project_id';

function readObject(value: unknown, key: string): unknown {
  if (typeof value !== 'object' || value === null) return undefined;
  return (value as Record<string, unknown>)[key];
}

/**
 * Pull the machine-readable code out of a rejected create-token call.
 *
 * TWO ENVELOPE SHAPES REACH THIS FUNCTION, and that is the whole reason it
 * exists. The two project failures use the NESTED shape
 * `{"error":{"message","type","code"}}`, while every other failure on the same
 * endpoint keeps the FLAT shape `{"error":"…"}`. A flat body has a string
 * where this reads an object, so it falls through to `null` and the caller
 * shows its generic message — no throw, no mis-parse.
 *
 * Reads structurally rather than through `instanceof EliteaApiError`: the
 * rejection travels through react-query, and a structural read cannot be
 * defeated by a second copy of the error class in the module graph.
 */
export function tokenProjectErrorCode(error: unknown): TokenProjectErrorCode | null {
  const code = readObject(nestedTokenError(error), 'code');
  if (code === 'project_forbidden' || code === 'invalid_project_id') return code;
  return null;
}

/** The `error` object of the nested envelope, or undefined for any other shape. */
function nestedTokenError(error: unknown): unknown {
  return readObject(readObject(readObject(error, 'failure'), 'body'), 'error');
}

/**
 * The four refusals `POST /api/v2/auth/token/` answers when the CALLING
 * credential may not mint the token (issue 2559, server
 * `internal/api/v2/auth/token_mint_policy.go`). They use the same nested
 * envelope as the project failures.
 *
 * - `token_mint_browser_session_required`: a token, not a sign-in, is calling.
 * - `token_mint_native_admin`: the desktop or mobile app, for an owner who
 *   holds administration permissions. Such an owner creates tokens in a
 *   browser.
 * - `token_mint_wider_than_caller`: the caller is limited to one project and
 *   the token would not be bound to it. The body carries that project as
 *   `error.project_id`.
 * - `token_mint_caller_unresolved`: the server could not check the calling
 *   sign-in or the owner's permissions.
 */
const TOKEN_MINT_REFUSAL_CODES = [
  'token_mint_browser_session_required',
  'token_mint_native_admin',
  'token_mint_wider_than_caller',
  'token_mint_caller_unresolved',
] as const;

type TokenMintRefusalCode = (typeof TOKEN_MINT_REFUSAL_CODES)[number];

export interface TokenMintRefusal {
  readonly code: TokenMintRefusalCode;
  /** The caller's own project binding, on `token_mint_wider_than_caller` only. */
  readonly projectId: number | null;
}

/** Pull a credential-kind refusal out of a rejected create-token call. */
export function tokenMintRefusal(error: unknown): TokenMintRefusal | null {
  const nested = nestedTokenError(error);
  const code = TOKEN_MINT_REFUSAL_CODES.find((known) => known === readObject(nested, 'code'));
  if (code === undefined) return null;
  const projectId = readObject(nested, 'project_id');
  return { code, projectId: typeof projectId === 'number' ? projectId : null };
}

/**
 * The refusal of the administration scopes on a create (ADR-0059 R1.3a,
 * server `internal/api/v2/auth/token_admin_scopes.go`): every code starts
 * with `token_scope`, and the two scope checks name the refused scopes in
 * `error.scopes`. The screen offers only what the server offered, so this
 * is reached when the person's permissions changed since the page loaded.
 */
export interface TokenScopeRefusal {
  readonly code: string;
  readonly scopes: readonly string[];
}

export function tokenScopeRefusal(error: unknown): TokenScopeRefusal | null {
  const nested = nestedTokenError(error);
  const code = readObject(nested, 'code');
  if (typeof code !== 'string' || !code.startsWith('token_scope')) return null;
  const scopes = readObject(nested, 'scopes');
  return {
    code,
    scopes: Array.isArray(scopes) ? scopes.filter((scope): scope is string => typeof scope === 'string') : [],
  };
}
