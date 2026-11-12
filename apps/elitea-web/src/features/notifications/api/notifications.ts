/**
 * features/notifications/api/notifications.ts — hand-written endpoint layer
 * for the `/notifications/*` domain (unit A11; manifest API-167..API-171,
 * ACT-053, ACT-054).
 *
 * WHY HAND-WRITTEN, NOT GENERATED: this resource has no OpenAPI schema
 * (confirmed against `services/elitea-main/api/openapi/v2.yaml` and
 * `src/shared/api/generated/**`, neither of which has a `notifications`
 * tag — `entities/notification`'s own doc comment records the same
 * finding). Per R-A5 ("every network call must go through a generated or
 * hand-registered endpoint … and appear in endpoints.manifest.json") these
 * 5 endpoints are hand-registered: every fetcher below calls `eliteaFetch`
 * (the same transport `shared/api/generated/**`'s orval hooks call — this
 * file adds NO new `fetch`/`XMLHttpRequest` site, so R-A1/R-A4 are
 * untouched), and `endpoints.manifest.json` carries one
 * `source:"handwritten"` entry per endpoint (this unit appended them — see
 * that file's own append convention comment).
 *
 * Ported from `apps/elitea-ui/src/api/notifications.js` (RTK Query). URL /
 * parameter / body shapes are byte-for-byte identical to the baseline
 * (API-167..171's acceptance text); the RTK-specific plumbing
 * (`providesTags`/`invalidatesTags`) has no equivalent here — that
 * responsibility moves to TanStack Query's cache-key/invalidation model at
 * the hook layer (`./useNotifications.ts`), per spec §2.3.
 *
 * `notificationRead`/`notificationDelete` (the SINGULAR, per-id endpoints)
 * are defined and exported for parity (API-168/API-169 are `must` manifest
 * items) but have ZERO call sites anywhere in `apps/elitea-ui` — every real
 * UI trigger (`NotificationListItem.jsx:68`, `NotificationList.jsx:50`,
 * `NotificationTable.jsx:148,166`) uses the BULK variants exclusively, even
 * for single-row actions (`ids: [notification.id]`). This is the old app's
 * own shipped state, not a defect this port introduces — reproduced as-is,
 * same as `usedBy: []` on a generated endpoint nobody calls yet.
 *
 * Response shapes are NOT documented by any spec, so `NotificationWire` is
 * read directly off `apps/elitea-ui/src/[fsd]/entities/notifications/
 * lib/helpers/{notification,notificationLegacy}.helpers.js` and
 * `.../ui/NotificationListItem.jsx`'s field usage, matching this codebase's
 * `x-elitea-passthrough` convention for a genuinely dynamic, per-event-type
 * `meta` payload (no single event type uses every key).
 */
import { eliteaFetch } from '@/shared/api/generated/mutator';

/**
 * `fetchData<T>` resolves to orval's enveloped shape
 * (`{data: T, status, headers}` — `@/shared/api/generated/mutator.ts`'s own
 * doc comment: "T is always the ENVELOPED type orval generates for every
 * operation … never the bare body"), matching what every *generated*
 * caller in `shared/api/generated/**` already receives. This unit's
 * hand-registered endpoints call `eliteaFetch` the same way (R-A5: no
 * second transport), so every fetcher below goes through this instead of
 * calling `eliteaFetch` directly, to unwrap `.data` at exactly one place
 * — same convention as `features/credentials/api/configurations.ts`'s
 * `fetchData<T>` (unit A7).
 */
async function fetchData<T>(
  url: string,
  options?: RequestInit,
  transport?: { readonly background?: boolean },
): Promise<T> {
  const envelope = await eliteaFetch<{ data: T }>(url, options, transport);
  return envelope.data;
}

/* ── wire types (snake_case, exactly as the old RTK Query layer read them) ── */

/**
 * Freeform per-event-type payload. Every field is read by SOME branch of
 * `notification.helpers.js`'s `resolveHref`/`parseMessage` (the
 * `meta.message`-driven path) or `notificationLegacy.helpers.js`'s
 * `parseInformation` (the pre-backfill fallback path) — see `../lib/
 * legacyText.ts` and `../lib/routes.ts` for the exact field-by-field
 * mapping. `[key: string]: unknown` passthrough matches this codebase's
 * convention for server-authored, genuinely dynamic payloads (e.g.
 * `features/credentials/api/configurations.ts`'s `ConfigSchemaNode`).
 */
export interface NotificationMetaWire {
  readonly message?: string;
  readonly conversation_id?: string;
  readonly message_id?: string;
  readonly toolkit_id?: string;
  readonly index_name?: string;
  readonly bucket_name?: string;
  readonly source_application_id?: string;
  readonly source_version_id?: string;
  readonly project_id?: string;
  readonly reason?: string;
  readonly error?: unknown;
  readonly token_name?: string;
  readonly rates_count?: number;
  readonly comments_count?: number;
  readonly replies_count?: number;
  readonly prompt_name?: string;
  readonly prompt_id?: string;
  readonly prompt_version_id?: string;
  readonly new_level?: string | number;
  readonly author_name?: string;
  readonly users?: readonly string[];
  readonly project_name?: string;
  readonly initiator_name?: string;
  readonly conversation_name?: string;
  readonly indexed?: number;
  readonly updated?: number;
  readonly reindex?: boolean;
  readonly initiator?: string;
  /* run_finished (ADR-0045 decision 5, U3 contract §1) — ids and an outcome, never content. */
  readonly outcome?: string;
  readonly limit?: string;
  readonly conversation_uuid?: string;
  readonly thread_id?: string;
  readonly origin?: string;
  readonly continuations?: number;
  readonly [key: string]: unknown;
}

/**
 * One row of `notificationList`'s response. Field usage evidence:
 * `id`/`event_type`/`meta`/`created_at`/`is_seen` —
 * `NotificationListItem.jsx:42,51,70,80,94,105,111`; `project_id` —
 * `NotificationListItemMessage.jsx:14` (fed to `resolveHref` as its third
 * argument).
 */
export interface NotificationWire {
  readonly id: string | number;
  readonly event_type: string;
  readonly meta?: NotificationMetaWire;
  readonly created_at: string;
  readonly is_seen: boolean;
  readonly project_id?: string | number;
  readonly [key: string]: unknown;
}

/** `notifications.js:13-26`'s response shape (`data?.rows`, `data?.total`). */
export interface NotificationListWire {
  readonly rows: readonly NotificationWire[];
  readonly total: number;
}

/* ── API-167: GET /notifications/notifications/prompt_lib/{projectId} ───── */

const NOTIFICATION_PAGE_SIZE = 20;

export interface ListNotificationsParams {
  readonly projectId: string | number;
  readonly page?: number;
  readonly pageSize?: number;
  /** Extra query params spread verbatim (old app: `...params`) — the one
   * observed caller (`NotificationList.jsx:38-40`) passes `{ only_new: true }`. */
  readonly params?: Readonly<Record<string, string | number | boolean>>;
  readonly sortBy?: string;
  readonly sortOrder?: string;
  readonly search?: string;
}

export function buildNotificationsListUrl(params: ListNotificationsParams): string {
  const { projectId, page = 0, pageSize = NOTIFICATION_PAGE_SIZE, params: extra = {}, sortBy, sortOrder, search } = params;
  const search_ = new URLSearchParams();
  for (const [key, value] of Object.entries(extra)) search_.append(key, String(value));
  search_.append('limit', String(pageSize));
  search_.append('offset', String(page * pageSize));
  if (sortBy !== undefined) search_.append('sort_by', sortBy);
  if (sortOrder !== undefined) search_.append('sort_order', sortOrder);
  if (search !== undefined && search !== '') search_.append('search', search);
  return `/notifications/notifications/prompt_lib/${projectId}?${search_.toString()}`;
}

export async function listNotifications(
  params: ListNotificationsParams,
  signal?: AbortSignal,
): Promise<NotificationListWire> {
  // `background`: this is the bell's own poll, running on every page the sidebar
  // mounts and asked for by nobody. Its 401 must be handed back as a failure,
  // never escalated into a re-auth — see `HttpRequestOptions.background`. The
  // MUTATIONS below deliberately do NOT set it: those are user actions, and a
  // 401 on one of them really does mean the session ended.
  return fetchData<NotificationListWire>(buildNotificationsListUrl(params), signal ? { signal } : {}, {
    background: true,
  });
}

/* ── API-168: PUT /notifications/notification/prompt_lib/{projectId}/{id} ── */

/** No UI call site in the baseline (see file header) — implemented for parity. */
export async function readNotification(
  projectId: string | number,
  id: string | number,
): Promise<unknown> {
  return fetchData<unknown>(`/notifications/notification/prompt_lib/${projectId}/${id}`, { method: 'PUT' });
}

/* ── API-169: DELETE /notifications/notification/prompt_lib/{projectId}/{id} ── */

/** No UI call site in the baseline (see file header) — implemented for parity. */
export async function deleteNotification(
  projectId: string | number,
  id: string | number,
): Promise<unknown> {
  return fetchData<unknown>(`/notifications/notification/prompt_lib/${projectId}/${id}`, { method: 'DELETE' });
}

/* ── API-170 / ACT-053: DELETE /notifications/notifications/prompt_lib/{projectId} ── */

/**
 * The Go handler decodes `ids` as integers (v2.yaml NotificationBulkDeleteRequest);
 * the UI carries ids as strings (`normalize.ts`), so a string id sent as-is is a 400.
 */
function toWireIds(ids: readonly (string | number)[]): number[] {
  return ids.map((id) => Number(id));
}

export async function bulkDeleteNotifications(
  projectId: string | number,
  ids: readonly (string | number)[],
): Promise<unknown> {
  return fetchData<unknown>(`/notifications/notifications/prompt_lib/${projectId}`, {
    method: 'DELETE',
    body: JSON.stringify({ ids: toWireIds(ids) }),
    headers: { 'Content-Type': 'application/json' },
  });
}

/* ── API-171 / ACT-054: PUT /notifications/notifications/prompt_lib/{projectId} ── */

/**
 * `ids` accepts the literal `'all'` — `NotificationList.jsx:50-54`'s
 * "mark all as read" popover action sends this instead of an id array; the
 * Go handler's contract for that sentinel is inferred from that call site
 * (no spec), not verified against handler source (out of this unit's
 * ownership fence).
 */
export async function bulkMarkSeenNotifications(
  projectId: string | number,
  ids: readonly (string | number)[] | 'all',
  isSeen: boolean,
): Promise<unknown> {
  return fetchData<unknown>(`/notifications/notifications/prompt_lib/${projectId}`, {
    method: 'PUT',
    body: JSON.stringify({ ids: ids === 'all' ? ids : toWireIds(ids), is_seen: isSeen }),
    headers: { 'Content-Type': 'application/json' },
  });
}
