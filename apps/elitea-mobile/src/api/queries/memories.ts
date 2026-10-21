import { useQuery, useQueryClient } from '@tanstack/react-query';
import { z } from 'zod';

import { sessionApiClient, type ApiClient } from '@/api/client';
import { isApiError, unwrap } from '@/api/errors';
import { queryKeys } from '@/api/queries/keys';

/**
 * The caller's own memories in a project (client contract 1.3:
 * listMemories, deleteMemory, clearMemories): facts the workspace recalls
 * before a chat turn starts. Only ever the caller's own entries. Read online
 * and kept in memory only: they are never written to the offline store, so a
 * signed-out or wiped device holds none.
 */

export interface Memory {
  id: string;
  content: string;
  tags: string[];
  enabled: boolean;
  /** The conversation it was saved from ("Remember this"), when known. */
  sourceConversationId: string | null;
  updatedAt: string;
}

const MEMORY_ID = /^[A-Za-z0-9_-]{1,64}$/;
/** The server refuses longer content (memories.MaxContentBytes); a longer row is cut for display. */
const MAX_CONTENT = 4000;

const Row = z.looseObject({
  id: z.string().regex(MEMORY_ID),
  content: z.string(),
  tags: z.array(z.string().max(200)).max(64).nullish(),
  enabled: z.boolean().optional(),
  source_conversation_id: z.string().max(64).nullish(),
  updated_at: z.string().max(64).optional(),
});
const Envelope = z.looseObject({ items: z.array(z.unknown()).nullish(), total: z.number().int().nonnegative() });

export type MemoryFailure = 'offline' | 'not_available' | 'not_allowed' | 'failed';

export class MemoryError extends Error {
  constructor(readonly reason: MemoryFailure) {
    super(`memories: ${reason}`);
    this.name = 'MemoryError';
  }
}

function failureOf(error: unknown): MemoryError {
  if (error instanceof MemoryError) return error;
  if (isApiError(error)) {
    if (error.kind === 'network' || error.kind === 'timeout') return new MemoryError('offline');
    if (error.status === 403) return new MemoryError('not_allowed');
    if (error.status === 404 || error.status === 501) return new MemoryError('not_available');
  }
  return new MemoryError('failed');
}

const FALLBACK = {
  load: "Couldn't load your memories. Try again.",
  delete: "That memory wasn't deleted. Try again.",
  clear: 'Nothing was cleared. Try again.',
} as const;

export function memoryErrorText(error: unknown, action: keyof typeof FALLBACK = 'load'): string {
  switch (error instanceof MemoryError ? error.reason : 'failed') {
    case 'offline':
      return 'Memory needs a connection to the workspace.';
    case 'not_available':
      return 'This workspace does not offer memory to mobile yet.';
    case 'not_allowed':
      return "You can't see memories in this project.";
    default:
      return FALLBACK[action];
  }
}

/** The list as the app shows it: malformed rows are dropped, never shown half-read. */
export function memoriesOf(data: unknown): Memory[] {
  const envelope = Envelope.safeParse(data);
  if (!envelope.success) throw new MemoryError('failed');
  const out: Memory[] = [];
  for (const raw of envelope.data.items ?? []) {
    const row = Row.safeParse(raw);
    if (!row.success || out.some((m) => m.id === row.data.id)) continue;
    out.push({
      id: row.data.id,
      content: row.data.content.slice(0, MAX_CONTENT),
      tags: (row.data.tags ?? []).filter((t) => t.trim() !== ''),
      enabled: row.data.enabled !== false,
      sourceConversationId: row.data.source_conversation_id ?? null,
      updatedAt: row.data.updated_at ?? '',
    });
  }
  return out;
}

const LIST = '/elitea_core/memories/prompt_lib/{project_id}' as const;

export async function fetchMemories(projectId: number, client: ApiClient = sessionApiClient(), signal?: AbortSignal): Promise<Memory[]> {
  try {
    const data = await unwrap(client.GET(LIST, { params: { path: { project_id: String(projectId) } }, signal }));
    return memoriesOf(data);
  } catch (error) {
    throw failureOf(error);
  }
}

/** Deletes one memory. Idempotent on the server: an id already gone is a 204 too. */
export async function deleteMemory(projectId: number, memoryId: string, client: ApiClient = sessionApiClient()): Promise<void> {
  if (!MEMORY_ID.test(memoryId)) throw new MemoryError('failed');
  try {
    await unwrap(
      client.DELETE('/elitea_core/memory/prompt_lib/{project_id}/{memory_id}', {
        params: { path: { project_id: String(projectId), memory_id: memoryId } },
      }),
    );
  } catch (error) {
    throw failureOf(error);
  }
}

/** Deletes every one of the caller's memories in the project; resolves to how many went. */
export async function clearMemories(projectId: number, client: ApiClient = sessionApiClient()): Promise<number> {
  try {
    const data = await unwrap(client.DELETE(LIST, { params: { path: { project_id: String(projectId) } } }));
    return typeof data?.removed === 'number' && data.removed >= 0 ? data.removed : 0;
  } catch (error) {
    throw failureOf(error);
  }
}

export function useMemories(projectId: number | null, enabled: boolean) {
  return useQuery({
    queryKey: queryKeys.memories(projectId ?? 0),
    queryFn: ({ signal }) => fetchMemories(projectId!, sessionApiClient(), signal),
    enabled: enabled && projectId !== null,
    retry: false,
    // Not kept around once the screen is gone.
    gcTime: 0,
  });
}

/**
 * Removes rows from the cached list (after a delete or a clear). A list fetch
 * already in flight is cancelled first: answered before the delete and
 * landing after this edit, it would put the rows back.
 */
export function useForgetMemories(projectId: number | null) {
  const queryClient = useQueryClient();
  return async (ids: readonly string[] | 'all'): Promise<void> => {
    if (projectId === null) return;
    await queryClient.cancelQueries({ queryKey: queryKeys.memories(projectId) });
    queryClient.setQueryData<Memory[]>(queryKeys.memories(projectId), (rows) =>
      rows === undefined ? rows : ids === 'all' ? [] : rows.filter((m) => !ids.includes(m.id)),
    );
  };
}
