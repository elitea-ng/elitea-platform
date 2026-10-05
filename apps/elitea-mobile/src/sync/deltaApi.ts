import { z } from 'zod';

import type { ApiClient } from '@/api/client';
import { unwrap } from '@/api/errors';
import type { ConversationMessage, ConversationSummary, Notification } from '@/db/repo';
import { log } from '@/lib/log';

/**
 * The three `changes_since` reads (ADR-0025 WP6; overlay at server 7665eb30f).
 * Every delta page is validated here, at the boundary: a malformed page
 * throws `DeltaShapeError` (the sync keeps its cursor and tries again later),
 * a malformed row is skipped (one bad row must not stall the list forever).
 */

/** Rows and tombstones each; the server's default and maximum. */
export const DELTA_LIMIT = 100;

export const TombstoneSchema = z.object({
  id: z.number().int().positive(),
  uuid: z.string().max(64).nullish(),
  /** `deleted` | `access_lost`; any other reason is a removal too. */
  reason: z.string().max(64),
  deleted_at: z.string().max(64),
});
export type Tombstone = z.infer<typeof TombstoneSchema>;

const Envelope = z.looseObject({
  tombstones: z.array(z.unknown()).nullish(),
  next_cursor: z.string().min(1).max(1024),
  has_more: z.boolean(),
});

const ConversationRow = z.looseObject({ id: z.number().int().positive() });
const MessageRow = z.looseObject({ id: z.union([z.string().regex(/^\d{1,16}$/), z.number().int().positive()]) });
const NotificationRow = z.looseObject({ id: z.number().int().positive(), is_seen: z.boolean().optional() });

export interface DeltaPage<Row> {
  rows: Row[];
  tombstones: Tombstone[];
  nextCursor: string;
  hasMore: boolean;
}

export class DeltaShapeError extends Error {
  constructor(readonly list: string) {
    super(`changes_since page of ${list} has an unexpected shape`);
    this.name = 'DeltaShapeError';
  }
}

function parsePage<Row>(list: string, body: unknown, rowsKey: 'rows' | 'items', rowSchema: z.ZodType): DeltaPage<Row> {
  const envelope = Envelope.safeParse(body);
  // A Go nil slice is marshalled as null: an empty page.
  const rawRows = (body as Record<string, unknown> | null)?.[rowsKey] ?? [];
  // No next_cursor: a server without incremental sync answered the legacy page.
  if (!envelope.success || !Array.isArray(rawRows)) throw new DeltaShapeError(list);
  let skipped = 0;
  const rows: Row[] = [];
  for (const raw of rawRows) {
    if (rowSchema.safeParse(raw).success) rows.push(raw as Row);
    else skipped++;
  }
  const tombstones: Tombstone[] = [];
  for (const raw of envelope.data.tombstones ?? []) {
    const parsed = TombstoneSchema.safeParse(raw);
    if (parsed.success) tombstones.push(parsed.data);
    else skipped++;
  }
  if (skipped > 0) log.warn('skipped malformed delta entries', { list, skipped });
  return { rows, tombstones, nextCursor: envelope.data.next_cursor, hasMore: envelope.data.has_more };
}

export async function fetchConversationChanges(
  client: ApiClient,
  projectId: number,
  cursor: string,
  signal?: AbortSignal,
): Promise<DeltaPage<ConversationSummary>> {
  const body = await unwrap(
    client.GET('/elitea_core/conversations/prompt_lib/{project_id}', {
      params: {
        path: { project_id: String(projectId) },
        // The cursor is bound to the filter: always the same `mine=true` list.
        query: { mine: true, limit: DELTA_LIMIT, changes_since: cursor },
      },
      signal,
    }),
  );
  return parsePage('conversations', body, 'rows', ConversationRow);
}

export async function fetchMessageChanges(
  client: ApiClient,
  projectId: number,
  conversationId: number,
  cursor: string,
  signal?: AbortSignal,
): Promise<DeltaPage<ConversationMessage>> {
  const body = await unwrap(
    client.GET('/elitea_core/messages/prompt_lib/{project_id}/{conversation_uuid}', {
      params: {
        // The transcript list takes the numeric id; its cursor is bound to it.
        path: { project_id: String(projectId), conversation_uuid: String(conversationId) },
        query: { limit: DELTA_LIMIT, changes_since: cursor },
      },
      signal,
    }),
  );
  return parsePage('messages', body, 'items', MessageRow);
}

export async function fetchNotificationChanges(
  client: ApiClient,
  projectId: number,
  cursor: string,
  signal?: AbortSignal,
): Promise<DeltaPage<Notification>> {
  const body = await unwrap(
    client.GET('/notifications/notifications/prompt_lib/{project_id}', {
      params: {
        path: { project_id: String(projectId) },
        // Any paging, filter or sort parameter next to changes_since is a 400.
        query: { limit: DELTA_LIMIT, changes_since: cursor },
      },
      signal,
    }),
  );
  return parsePage('notifications', body, 'rows', NotificationRow);
}
