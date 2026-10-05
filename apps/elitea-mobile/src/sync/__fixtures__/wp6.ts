/**
 * `changes_since` delta pages in the server's real wire shapes (elitea-main
 * at 7665eb30f, ADR-0025 WP6; documented at 04cab926b):
 *
 * - conversation rows are the legacy list row (zoneless microsecond times,
 *   `duration: -1`, `meta` always an object); `total` is the whole list now;
 * - transcript rows carry the group id AS A STRING, `uid`, `role`,
 *   `content`; `message_items` only for attachments; `is_streaming` /
 *   `task_id` only while in flight; tombstones carry the group id as an int;
 * - notification rows as the legacy list (RFC 3339 seconds);
 * - tombstones `{id, uuid (nullable), reason: deleted | access_lost, deleted_at}`;
 * - `next_cursor` is base64url(JSON) of `{v, s, scope, t, i, dt, di}`, opaque
 *   to the client; Go nil slices arrive as `null`.
 */

export function cursor(stream: 'conv' | 'msg' | 'notif', scope: string, at: string, id: number): string {
  const json = JSON.stringify({ v: 1, s: stream, scope, t: at, i: id, dt: at, di: 0 });
  return btoa(json).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

export const conversationRow = (id: number, overrides: Record<string, unknown> = {}) => ({
  id,
  name: `Conversation ${id}`,
  created_at: '2026-10-01T09:00:00.000000',
  updated_at: '2026-10-04T12:00:00.123456',
  meta: {},
  duration: -1,
  message_groups_count: 2,
  ...overrides,
});

export const messageRow = (id: number, overrides: Record<string, unknown> = {}) => ({
  id: String(id),
  uid: `00000000-0000-4000-8000-${String(id).padStart(12, '0')}`,
  conversation_id: '42',
  role: id % 2 === 1 ? 'user' : 'assistant',
  content: `message ${id}`,
  content_type: 'text',
  author_participant_id: id % 2 === 1 ? 1 : 2,
  metadata: {},
  created_at: `2026-10-04T12:00:${String(id % 60).padStart(2, '0')}Z`,
  ...overrides,
});

export const notificationRow = (id: number, overrides: Record<string, unknown> = {}) => ({
  id,
  uuid: `10000000-0000-4000-8000-${String(id).padStart(12, '0')}`,
  is_seen: false,
  project_id: 7,
  user_id: 3,
  meta: { conversation_id: 42 },
  created_at: '2026-10-04T12:00:00Z',
  updated_at: null,
  event_type: 'agent_finished',
  ...overrides,
});

export const tombstone = (id: number, reason: 'deleted' | 'access_lost' = 'deleted', uuid: string | null = null) => ({
  id,
  uuid,
  reason,
  deleted_at: '2026-10-04T12:00:01.5Z',
});

export function conversationDelta(
  rows: unknown[] | null,
  options: { tombstones?: unknown[] | null; next?: string; hasMore?: boolean; total?: number } = {},
) {
  return {
    total: options.total ?? (rows?.length ?? 0),
    rows,
    tombstones: options.tombstones ?? [],
    next_cursor: options.next ?? cursor('conv', '7/3f2a9c1b0d4e', '2026-10-04T11:59:55Z', 0),
    has_more: options.hasMore ?? false,
  };
}

export function messageDelta(
  items: unknown[] | null,
  options: { tombstones?: unknown[] | null; next?: string; hasMore?: boolean; total?: number } = {},
) {
  return {
    items,
    total: options.total ?? (items?.length ?? 0),
    tombstones: options.tombstones ?? null,
    next_cursor: options.next ?? cursor('msg', '7/42', '2026-10-04T11:59:55Z', 0),
    has_more: options.hasMore ?? false,
  };
}

export function notificationDelta(
  rows: unknown[] | null,
  options: { tombstones?: unknown[] | null; next?: string; hasMore?: boolean } = {},
) {
  return {
    total: rows?.length ?? 0,
    rows,
    tombstones: options.tombstones ?? [],
    next_cursor: options.next ?? cursor('notif', '3', '2026-10-04T11:59:55Z', 0),
    has_more: options.hasMore ?? false,
  };
}

/** The 400 / 410 bodies of a refused cursor. */
export const cursorExpired = {
  error: 'sync_cursor_expired',
  message: 'the sync cursor is older than the tombstone retention; resync from changes_since=0',
};
export const cursorInvalid = { error: 'invalid_sync_cursor', message: 'changes_since is not a cursor this list issued' };
