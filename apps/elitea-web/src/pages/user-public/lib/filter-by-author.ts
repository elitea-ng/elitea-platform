/**
 * Client-side author filter — a REQUIRED deviation from the baseline, not a
 * style choice. Reproduces the effect of the baseline's server-side
 * `author_id` filter (`apps/elitea-ui/src/pages/UserPublic/UserPublic.jsx:87,101,130,144,153`
 * — every list query sends `author_id: authorId`) client-side instead,
 * because the Go handler this app's generated client targets does not
 * accept it:
 *
 * `internal/api/v2/applications/handler.go:71-107` (read directly, unit
 * A12) parses only `limit`, `offset`, `query`/`search`, `tags`, `folder_id`,
 * `agents_type` from the request — no `author_id`, no `statuses`. Confirmed
 * against three independent sources: the OpenAPI spec's declared
 * parameters (`Limit`/`Offset` only —
 * `services/elitea-main/api/openapi/v2.yaml:674-724`), the orval-generated
 * `ListApplicationsParams`/`ListPublicApplicationsParams` zod schemas
 * (`src/shared/api/generated/model/listApplicationsParams.zod.ts`,
 * `listPublicApplicationsParams.zod.ts`), and the handler source itself. A
 * live parity defect flagged for operator triage in the A12 report — see
 * that report for whether this is a genuine Go-side regression from the
 * pre-migration (legacy) backend or was always the case.
 *
 * Only applicable to the `Application` shape (has `authors`/`owner_id`).
 * The public-catalog `PublicApplicationSummary` shape carries no author
 * field at all (see `ApplicationsPanel.tsx`'s public-view-mode handling).
 */
/**
 * `authors`/`owner_id` are typed with an explicit `| undefined` union
 * (rather than a bare `?:`) so this structurally matches the generated
 * `Application` wire type under `exactOptionalPropertyTypes: true` — that
 * type's zod-generated `.optional()` fields are `T | undefined`, not the
 * narrower "may be absent, but if present must be exactly T" `exactOptionalPropertyTypes`
 * gives a bare `field?: T`.
 */
export interface AuthorFilterable {
  readonly authors?: readonly { readonly id: string }[] | undefined;
  readonly owner_id?: string | undefined;
}

export function matchesAuthor(item: AuthorFilterable, authorId: string): boolean {
  if (authorId === '') return true;
  if (item.authors?.some((author) => author.id === authorId) === true) return true;
  return item.owner_id === authorId;
}

export function filterByAuthor<T extends AuthorFilterable>(items: readonly T[], authorId: string): T[] {
  if (authorId === '') return [...items];
  return items.filter((item) => matchesAuthor(item, authorId));
}
