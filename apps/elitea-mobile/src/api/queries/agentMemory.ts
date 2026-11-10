import { useInfiniteQuery, useQueryClient, type InfiniteData } from '@tanstack/react-query';
import { z } from 'zod';

import { sessionApiClient, type ApiClient } from '@/api/client';
import { isApiError } from '@/api/errors';
import { queryKeys } from '@/api/queries/keys';

/**
 * What an agent remembers in a project (ADR-0050 decision 1b, client contract 1.44:
 * listAgentMemory, deleteAgentMemoryEntry, setAgentMemoryOptOut). The server decides what the
 * caller sees: a note about another person is listed only for that person and the agent's
 * managers. Delete is offered on a note about the caller, or on any note when the list says
 * `can_delete`. Read online and kept in memory only, like the caller's own memories.
 *
 * The app reads, deletes and opts out; editing, pinning, approving, clearing and the history
 * are on the web and desktop Memory tab.
 */

export type AgentMemoryKind = 'fact' | 'preference' | 'episode' | 'cursor';
export type AgentMemoryStatus = 'pending' | 'active' | 'archived';

export interface AgentMemoryEntry {
  id: number;
  kind: AgentMemoryKind | string;
  content: string;
  /** The person the note is about; null for a note about the project or the work. */
  subjectUserId: number | null;
  pinned: boolean;
  status: AgentMemoryStatus | string;
  writtenByKind: 'agent' | 'person' | string;
  writtenByUserId: number | null;
  /** The conversation it was written in, when known. */
  conversationUuid: string | null;
  createdAt: string;
  updatedAt: string;
}

export interface AgentMemoryPage {
  rows: AgentMemoryEntry[];
  total: number;
  /** User id → name, for the people the rows are about or were written by. */
  people: Record<number, string>;
  canDelete: boolean;
  optedOut: boolean;
  /** How many rows this page started after (the request's offset). */
  offset: number;
  /** How many rows the server answered (before malformed ones were dropped). */
  received: number;
}

/** Client contract 1.44 tags the agent memory routes `client`. */
export const AGENT_MEMORY_CONTRACT = { major: 1, minor: 44 } as const;

export const AGENT_MEMORY_PAGE_SIZE = 50;
/** The server's own bound (AgentMemoryEntry.content maxLength). */
const MAX_CONTENT = 2048;

const Id = z.number().int().positive().max(Number.MAX_SAFE_INTEGER);
const Row = z.looseObject({
  id: Id,
  kind: z.string().max(32),
  content: z.string(),
  subject_user_id: Id.nullish(),
  pinned: z.boolean().optional(),
  status: z.string().max(32),
  written_by_kind: z.string().max(32).optional(),
  written_by_user_id: Id.nullish(),
  conversation_uuid: z.string().max(64).nullish(),
  created_at: z.string().max(64).optional(),
  updated_at: z.string().max(64).optional(),
});
const Person = z.looseObject({ id: Id, name: z.string().max(256) });
const Envelope = z.looseObject({
  rows: z.array(z.unknown()).nullish(),
  total: z.number().int().nonnegative(),
  people: z.array(z.unknown()).nullish(),
  can_delete: z.boolean().optional(),
  opted_out: z.boolean().optional(),
});

export type AgentMemoryFailure = 'offline' | 'not_available' | 'refused' | 'failed';

/**
 * A failed call. `message` is the server's own sentence for a refusal (`{error, code}`), else
 * the app's; `code` is the server's code when it sent one.
 */
export class AgentMemoryError extends Error {
  constructor(
    readonly reason: AgentMemoryFailure,
    message: string,
    readonly code?: string,
    readonly status?: number,
  ) {
    super(message);
    this.name = 'AgentMemoryError';
  }
}

export const AGENT_MEMORY_COPY = {
  offline: "Agent memory needs a connection to your workspace.",
  notAvailable: "This workspace doesn't show agent memory in the app yet.",
  load: "Couldn't load what this agent remembers. Try again.",
  delete: "That note wasn't deleted. Try again.",
  optOut: "The setting wasn't saved. Try again.",
} as const;

/** The refusal's own words, when the body is `{error, code}` (agentmemory/handler.go writeError). */
function refusalOf(body: unknown): { message: string; code?: string } | null {
  if (body === null || typeof body !== 'object') return null;
  const { error, code } = body as { error?: unknown; code?: unknown };
  if (typeof error !== 'string' || error.trim() === '') return null;
  return { message: error.trim().slice(0, 500), code: typeof code === 'string' && code.length <= 64 ? code : undefined };
}

/** Turns a non-2xx answer into an AgentMemoryError in the server's words when it gave some. */
function httpFailure(response: Response, body: unknown, fallback: string): AgentMemoryError {
  // 404/501 on the list: a server without the routes. A 404 on a delete is the server's own
  // "no such entry" and keeps its sentence.
  const refusal = refusalOf(body);
  if (refusal) return new AgentMemoryError('refused', refusal.message, refusal.code, response.status);
  if (response.status === 501) return new AgentMemoryError('not_available', AGENT_MEMORY_COPY.notAvailable, undefined, 501);
  return new AgentMemoryError('failed', fallback, undefined, response.status);
}

function thrownFailure(error: unknown, fallback: string): AgentMemoryError {
  if (error instanceof AgentMemoryError) return error;
  if (isApiError(error)) {
    if (error.kind === 'network' || error.kind === 'timeout') return new AgentMemoryError('offline', AGENT_MEMORY_COPY.offline);
  }
  return new AgentMemoryError('failed', fallback);
}

/** The text a failure shows: always the error's message (the server's or the app's). */
export function agentMemoryErrorText(error: unknown, fallback: string = AGENT_MEMORY_COPY.load): string {
  return error instanceof AgentMemoryError ? error.message : fallback;
}

/** One page as the app shows it: malformed rows are dropped, never shown half-read. */
export function agentMemoryPageOf(data: unknown, offset: number): AgentMemoryPage {
  const envelope = Envelope.safeParse(data);
  if (!envelope.success) throw new AgentMemoryError('failed', AGENT_MEMORY_COPY.load);
  const rows: AgentMemoryEntry[] = [];
  for (const raw of envelope.data.rows ?? []) {
    const row = Row.safeParse(raw);
    if (!row.success || rows.some((r) => r.id === row.data.id)) continue;
    rows.push({
      id: row.data.id,
      kind: row.data.kind,
      content: row.data.content.slice(0, MAX_CONTENT),
      subjectUserId: row.data.subject_user_id ?? null,
      pinned: row.data.pinned === true,
      status: row.data.status,
      writtenByKind: row.data.written_by_kind ?? 'agent',
      writtenByUserId: row.data.written_by_user_id ?? null,
      conversationUuid: row.data.conversation_uuid ?? null,
      createdAt: row.data.created_at ?? '',
      updatedAt: row.data.updated_at ?? '',
    });
  }
  const people: Record<number, string> = {};
  for (const raw of envelope.data.people ?? []) {
    const person = Person.safeParse(raw);
    if (person.success && person.data.name.trim() !== '') people[person.data.id] = person.data.name.trim();
  }
  return {
    rows,
    total: envelope.data.total,
    people,
    canDelete: envelope.data.can_delete === true,
    optedOut: envelope.data.opted_out === true,
    offset,
    received: envelope.data.rows?.length ?? 0,
  };
}

/** The next page's offset, or undefined when every row has been read. */
export function nextAgentMemoryOffset(last: AgentMemoryPage): number | undefined {
  const next = last.offset + last.received;
  return last.received > 0 && next < last.total ? next : undefined;
}

const LIST = '/elitea_core/agent_memory/prompt_lib/{project_id}/{application_id}' as const;

export async function fetchAgentMemoryPage(
  projectId: number,
  applicationId: number,
  offset: number,
  client: ApiClient = sessionApiClient(),
  signal?: AbortSignal,
): Promise<AgentMemoryPage> {
  try {
    const { data, error, response } = await client.GET(LIST, {
      params: {
        path: { project_id: String(projectId), application_id: applicationId },
        query: { limit: AGENT_MEMORY_PAGE_SIZE, ...(offset > 0 ? { offset } : {}) },
      },
      signal,
    });
    if (!response.ok) {
      // A server without the route answers the router's 404 (`{error: 'not found'}`, no code, or
      // no body at all); the memory handler's own 404 always carries `code`.
      if (response.status === 404 && refusalOf(error)?.code === undefined) {
        throw new AgentMemoryError('not_available', AGENT_MEMORY_COPY.notAvailable, undefined, 404);
      }
      throw httpFailure(response, error, AGENT_MEMORY_COPY.load);
    }
    return agentMemoryPageOf(data, offset);
  } catch (error) {
    throw thrownFailure(error, AGENT_MEMORY_COPY.load);
  }
}

/** Deletes one note. A refusal (403, 404) rejects with the server's own sentence. */
export async function deleteAgentMemoryEntry(
  projectId: number,
  applicationId: number,
  entryId: number,
  client: ApiClient = sessionApiClient(),
): Promise<void> {
  try {
    const { error, response } = await client.DELETE(`${LIST}/{entry_id}`, {
      params: { path: { project_id: String(projectId), application_id: applicationId, entry_id: entryId } },
    });
    if (!response.ok) throw httpFailure(response, error, AGENT_MEMORY_COPY.delete);
  } catch (error) {
    throw thrownFailure(error, AGENT_MEMORY_COPY.delete);
  }
}

/**
 * "Remember things about me": `optedOut` true deletes what the agent holds about the caller and
 * stops it writing more. Resolves to the stored choice.
 */
export async function setAgentMemoryOptOut(
  projectId: number,
  applicationId: number,
  optedOut: boolean,
  client: ApiClient = sessionApiClient(),
): Promise<boolean> {
  try {
    const { data, error, response } = await client.PUT('/elitea_core/agent_memory_opt_out/prompt_lib/{project_id}/{application_id}', {
      params: { path: { project_id: String(projectId), application_id: applicationId } },
      body: { opted_out: optedOut },
    });
    if (!response.ok) throw httpFailure(response, error, AGENT_MEMORY_COPY.optOut);
    return typeof data?.opted_out === 'boolean' ? data.opted_out : optedOut;
  } catch (error) {
    throw thrownFailure(error, AGENT_MEMORY_COPY.optOut);
  }
}

/** Whether the caller may delete this note: their own, or any when the list allows it. */
export function canDeleteEntry(entry: Pick<AgentMemoryEntry, 'subjectUserId'>, canDelete: boolean, selfUserId: number | null): boolean {
  return canDelete || (selfUserId !== null && entry.subjectUserId === selfUserId);
}

/**
 * The loaded pages without one deleted note. Every page's total drops by one; the page that held
 * it counts one row fewer and every later page starts one row earlier, so the next page's offset
 * (`nextAgentMemoryOffset` of the last page) does not skip the row that moved up on the server.
 */
export function withoutEntry(pages: readonly AgentMemoryPage[], entryId: number): AgentMemoryPage[] {
  const at = pages.findIndex((page) => page.rows.some((r) => r.id === entryId));
  if (at < 0) return [...pages];
  return pages.map((page, index) => ({
    ...page,
    rows: index === at ? page.rows.filter((r) => r.id !== entryId) : page.rows,
    total: Math.max(0, page.total - 1),
    received: index === at ? Math.max(0, page.received - 1) : page.received,
    offset: index > at ? Math.max(0, page.offset - 1) : page.offset,
  }));
}

export function useAgentMemory(projectId: number | null, applicationId: number | null, enabled: boolean) {
  return useInfiniteQuery({
    queryKey: queryKeys.agentMemory(projectId ?? 0, applicationId ?? 0),
    enabled: enabled && projectId !== null && applicationId !== null,
    retry: false,
    // Not kept around once the screen is gone.
    gcTime: 0,
    initialPageParam: 0,
    queryFn: ({ pageParam, signal }) => fetchAgentMemoryPage(projectId as number, applicationId as number, pageParam, sessionApiClient(), signal),
    getNextPageParam: nextAgentMemoryOffset,
  });
}

/**
 * Edits the cached pages after a write: drops a deleted note (and counts it out of the total),
 * or stores the opt-out choice. A list fetch in flight is cancelled first: answered before the
 * write and landing after this edit, it would undo it.
 */
export function useAgentMemoryCache(projectId: number | null, applicationId: number | null) {
  const queryClient = useQueryClient();
  const key = queryKeys.agentMemory(projectId ?? 0, applicationId ?? 0);
  const edit = async (change: (pages: AgentMemoryPage[]) => AgentMemoryPage[]) => {
    if (projectId === null || applicationId === null) return;
    await queryClient.cancelQueries({ queryKey: key });
    queryClient.setQueryData<InfiniteData<AgentMemoryPage, number>>(key, (data) =>
      data === undefined ? data : { ...data, pages: change(data.pages) },
    );
  };
  return {
    forget: (entryId: number) => edit((pages) => withoutEntry(pages, entryId)),
    setOptedOut: (optedOut: boolean) => edit((pages) => pages.map((page) => ({ ...page, optedOut }))),
    /** Re-reads every page (after an opt-out deleted the notes about the caller). */
    refresh: () => queryClient.invalidateQueries({ queryKey: key }),
  };
}
