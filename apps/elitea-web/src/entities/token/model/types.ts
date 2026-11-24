/**
 * Token domain type — a personal access token. No OpenAPI schema exists for
 * this resource (settings domain, not in the W2 manifest).
 *
 * Evidence: apps/elitea-ui/src/api/auth.js:51-77 — `tokenList` (GET, the
 * FULL token value is present in the list response — the old app truncates
 * for display, it does not withhold the value server-side, see
 * `TokensTable.jsx:284` `row.token.substring(row.token.length - 4)`);
 * `tokenCreate` (POST, body `{name, expires}`); `tokenDelete` (DELETE
 * `{uuid}`). apps/elitea-ui/src/common/constants.js:476-477 —
 * `DEFAULT_TOKEN_EXPIRATION_DAYS = 30`,
 * `EXPIRATION_MEASURES = ['never','days','weeks','hours','minutes']`.
 *
 * Two distinct `expires` shapes exist — do not conflate them:
 * - the CREATE-REQUEST body's `expires` is `null` ("never") or
 *   `{measure, value}` (`CreatePersonalToken.jsx:43-47`) — the Go backend
 *   resolves this into an absolute expiry server-side;
 * - the PERSISTED token record's `expires` (as returned by `tokenList`) is
 *   a date string or `null`, confirmed by `TokensTable.jsx:38-40` passing
 *   `row.expires` straight into `calculateExpiryInDays`
 *   (apps/elitea-ui/src/common/utils.jsx:691-705), which does
 *   `new Date(expiration).getTime()`.
 */
type TokenExpirationMeasure = 'days' | 'weeks' | 'hours' | 'minutes';

/** The CREATE-REQUEST `expires` shape. `null` means "never expires". */
export type TokenExpirationRequest = null | { readonly measure: TokenExpirationMeasure; readonly value: number };

export interface PersonalAccessToken {
  readonly uuid: string;
  readonly name: string;
  /** The full token value, as returned by the API — masking is a display concern (see selectors). */
  readonly token: string;
  /** ISO 8601 date-time, or `null` for "never expires". */
  readonly expires: string | null;
  /**
   * The project this key bills, or `null` when the key is UNBOUND
   * (`spec-llm-project-scope` §4: `GET /api/v2/auth/token/` and
   * `GET /api/v2/auth/token/{uuid}` return `project_id`, JSON `null` when
   * there is no binding).
   *
   * Declared OPTIONAL, not required: a token record read from a backend that
   * predates the binding carries no such field at all, and an unbound token
   * carries `null`. Both mean the same thing to every consumer — "this key
   * bills nothing in particular" — so treat `undefined` and `null` alike.
   */
  readonly project_id?: number | null;
  /**
   * The token's administration access (ADR-0059 decision 7). Optional for
   * the same reason as `project_id`: an older backend sends none, which
   * means `none`.
   */
  readonly admin_access?: TokenAdminAccess;
  /** The scopes of a `scoped` token, sorted; empty otherwise. */
  readonly admin_scopes?: readonly string[];
  /** The date (YYYY-MM-DD) an `inherit` token ends; absent until the sunset date is set. */
  readonly inherit_ends_on?: string;
}

/** `inherit` is a backfill value only: no screen offers it. */
type TokenAdminAccess = 'none' | 'scoped' | 'inherit';
