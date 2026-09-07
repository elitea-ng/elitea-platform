/**
 * "Last login" for Settings › Profile.
 *
 * WHY THIS IS NOT READ OFF `GET /social/author`. That endpoint is the
 * caller's own profile, and its response carries `id`, `name`, `email`,
 * `avatar`, `description`, `personal_project_id`, `personalization`,
 * `default_context_management`, `default_summarization` and `provider_refs`
 * — no login timestamp of any name. `ProfileIdentity` therefore used to drop
 * the row entirely and say so in a comment, on the belief that the only
 * carrier of `last_login` was the ADMIN users list, which a normal user
 * cannot call.
 *
 * That belief was too narrow. `GET /social/authors/{projectID}` — the
 * project author list, permission `models.social.authors.get`, callable by
 * any member — emits `last_login` per row
 * (`internal/api/v2/social/authors.go`). The caller is always a member of
 * their OWN personal project, and that project has exactly one member: the
 * caller. So the caller's own last login is one ordinary, non-admin request
 * away. Verified against a running stack, signed in as a non-admin-scoped
 * session: `GET /api/v2/social/authors/2` → 200
 * `[{"id":2,"email":"…","name":"…","last_login":"Sun, 06 Sep 2026 14:36:57
 * GMT","suspended":false,"avatar":null}]`.
 *
 * THE FIELD IS UNDOCUMENTED, SO IT IS READ DEFENSIVELY. `v2.yaml` models
 * this operation's rows as `SocialAuthorSummary` (`id`, `name`, `email`,
 * `avatar`, `description`) — the shape of the LEGACY handler — so the
 * generated client's type carries neither `last_login` nor `suspended`. A
 * deployment that still serves the legacy handler answers without the field.
 * `selectLastLogin` narrows at runtime and returns `undefined` there rather
 * than throwing or inventing a timestamp, and the row still renders with an
 * empty value — which is exactly what the reference does
 * (`Profile.jsx:67-70`, `dateFormatter(last_login) || ''`), so the row's
 * presence never depends on the deployment.
 */

/** RFC1123 GMT in the production handler; RFC3339 in other emitters. `Date` parses both. */
interface SocialAuthorRow {
  readonly id?: number | string;
  readonly last_login?: string | null;
}

function isRow(value: unknown): value is SocialAuthorRow {
  return typeof value === 'object' && value !== null;
}

/**
 * The caller's `last_login`, raw, from a `GET /social/authors/{projectID}`
 * body. `undefined` when the body is not a list, when no row matches, or
 * when the matching row carries no timestamp.
 *
 * Ids are compared as strings: `/social/author` returns `id` as a STRING
 * ("2") while `/social/authors/{id}` returns it as a NUMBER (2), so a strict
 * comparison of the two never matches.
 */
export function selectLastLogin(rows: unknown, userId: string | undefined): string | undefined {
  if (!Array.isArray(rows) || userId === undefined || userId === '') return undefined;
  for (const row of rows) {
    if (!isRow(row)) continue;
    if (row.id === undefined || String(row.id) !== String(userId)) continue;
    return typeof row.last_login === 'string' && row.last_login !== '' ? row.last_login : undefined;
  }
  return undefined;
}

/**
 * The reference's `dateFormatter` (`settings/lib/helpers/dateFormatter.helpers.js`):
 * `new Date(value).toLocaleString()`. A live deployment renders
 * `9/6/2026, 10:26:43 AM`, i.e. the viewer's locale, not a fixed pattern.
 *
 * An unparseable string yields `Invalid Date` from `toLocaleString`, which
 * would be worse than a blank; that case returns `''` instead.
 */
export function formatLastLogin(value: string | null | undefined): string {
  if (value === undefined || value === null || value === '') return '';
  const parsed = new Date(value);
  if (Number.isNaN(parsed.getTime())) return '';
  return parsed.toLocaleString();
}
