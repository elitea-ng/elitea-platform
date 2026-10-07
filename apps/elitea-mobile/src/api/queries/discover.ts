import { useInfiniteQuery, useQuery, useQueryClient, type InfiniteData } from '@tanstack/react-query';
import { useCallback, useSyncExternalStore } from 'react';
import { z } from 'zod';

import { sessionApiClient, type ApiClient } from '@/api/client';
import { unwrap } from '@/api/errors';
import type { components } from '@/api/generated/elitea.gen';
import { queryKeys } from '@/api/queries/keys';
import { addParticipants } from '@/api/queries/participants';

/**
 * Discover (client contract 1.3): the workspace's public agent catalogue.
 *
 * - listPublicApplications answers `{rows, total}`; `total` counts the
 *   FILTERED set, so the list pages by offset until it is reached. Each row
 *   carries the public `project_id` and the published `version_id` (ids are
 *   numeric strings on the wire).
 * - getPublicApplication adds the version's welcome message and
 *   conversation starters. It is optional: the details screen works from the
 *   catalogue row alone when it fails.
 * - Starting a chat follows the server's 1.3 semantics (elitea-platform PR
 *   #1090): createConversation in the caller's ACTIVE project, then
 *   addConversationParticipants with the agent (`entity_meta.project_id` =
 *   the public project, `entity_settings.version_id` = the published version).
 *
 * The caller gates all of it on `speaksContract(discovery, 1, 3)`.
 */

export const DISCOVER_PAGE_SIZE = 20;
/** Bounds on what one catalogue row may carry into the UI. */
const MAX_TAGS = 12;
const MAX_STARTERS = 6;

const NumericId = z
  .union([z.number(), z.string().regex(/^\d{1,15}$/)])
  .transform(Number)
  .refine((n) => Number.isSafeInteger(n) && n > 0);

const Tag = z.looseObject({ id: z.union([z.number(), z.string()]), name: z.string().min(1).max(100) });

const Meta = z
  .looseObject({
    category: z.string().max(200).nullish().catch(null),
    icon_meta: z.looseObject({ url: z.string().max(2000).nullish() }).nullish().catch(null),
  })
  .nullish()
  .catch(null);

const Row = z.looseObject({
  project_id: NumericId,
  id: NumericId,
  name: z.string().max(500),
  description: z.string().max(10_000).nullish(),
  version_id: NumericId,
  version_name: z.string().max(200).nullish(),
  agent_type: z.string().max(100).nullish(),
  meta: Meta,
  tags: z.array(z.unknown()).max(1000).nullish().catch(null),
  likes: z.number().int().nonnegative().nullish().catch(null),
});

const Envelope = z.looseObject({ rows: z.array(z.unknown()).max(1000), total: z.number().int().nonnegative().nullish() });

export interface PublicAgentTag {
  id: string;
  name: string;
}

export interface PublicAgent {
  id: number;
  /** The public project the agent lives in (a participant's `entity_meta.project_id`). */
  publicProjectId: number;
  name: string;
  description: string;
  /** The published version a chat runs. */
  versionId: number;
  versionName: string;
  /** `pipeline` or an agent type such as `openai`. */
  agentType: string;
  category: string | null;
  tags: PublicAgentTag[];
  /** The icon's path or URL as the server stores it; render it through `publicAgentIconUrl`. */
  iconPath: string | null;
  likes: number;
}

export interface PublicAgentPage {
  agents: PublicAgent[];
  total: number;
  offset: number;
  /** Rows the server sent, valid or not: paging advances by these. */
  received: number;
}

function tagsOf(raw: unknown[] | null | undefined): PublicAgentTag[] {
  const out: PublicAgentTag[] = [];
  for (const item of raw ?? []) {
    const tag = Tag.safeParse(item);
    if (!tag.success) continue;
    const name = tag.data.name.trim();
    if (name === '' || out.some((t) => t.name === name)) continue;
    out.push({ id: String(tag.data.id), name });
    if (out.length >= MAX_TAGS) break;
  }
  return out;
}

/** One catalogue row as an agent, or null when it cannot be chatted with (no ids, no name). */
export function publicAgentOf(raw: unknown): PublicAgent | null {
  const row = Row.safeParse(raw);
  if (!row.success) return null;
  const name = row.data.name.trim();
  if (name === '') return null;
  const icon = row.data.meta?.icon_meta?.url?.trim();
  const category = row.data.meta?.category?.trim();
  return {
    id: row.data.id,
    publicProjectId: row.data.project_id,
    name,
    description: row.data.description?.trim() ?? '',
    versionId: row.data.version_id,
    versionName: row.data.version_name?.trim() ?? '',
    agentType: row.data.agent_type?.trim() ?? '',
    category: category ? category : null,
    tags: tagsOf(row.data.tags),
    iconPath: icon ? icon : null,
    likes: row.data.likes ?? 0,
  };
}

export function publicAgentPageOf(data: unknown, offset: number): PublicAgentPage {
  const envelope = Envelope.safeParse(data);
  if (!envelope.success) return { agents: [], total: 0, offset, received: 0 };
  const agents: PublicAgent[] = [];
  for (const raw of envelope.data.rows) {
    const agent = publicAgentOf(raw);
    if (agent !== null) agents.push(agent);
  }
  const received = envelope.data.rows.length;
  return { agents, total: envelope.data.total ?? offset + received, offset, received };
}

/** Offset of the next page, or undefined when the (filtered) catalogue is exhausted. */
export function nextPublicAgentOffset(last: PublicAgentPage): number | undefined {
  const next = last.offset + last.received;
  if (last.received === 0 || next >= last.total) return undefined;
  return next;
}

/** Flattens pages, dropping an agent a later page repeats (the catalogue changed while paging). */
export function flattenPublicAgentPages(pages: readonly PublicAgentPage[] | undefined): PublicAgent[] {
  const seen = new Set<number>();
  const out: PublicAgent[] = [];
  for (const page of pages ?? []) {
    for (const agent of page.agents) {
      if (seen.has(agent.id)) continue;
      seen.add(agent.id);
      out.push(agent);
    }
  }
  return out;
}

export async function fetchPublicAgents(
  search: string,
  offset: number,
  client: ApiClient = sessionApiClient(),
  signal?: AbortSignal,
): Promise<PublicAgentPage> {
  const query = search.trim();
  const data = await unwrap(
    client.GET('/elitea_core/public_applications/prompt_lib', {
      params: {
        query: {
          limit: DISCOVER_PAGE_SIZE,
          offset,
          sort_by: 'likes',
          sort_order: 'desc',
          ...(query !== '' ? { query } : {}),
        },
      },
      signal,
    }),
  );
  return publicAgentPageOf(data, offset);
}

/**
 * The catalogue for the Discover screen. Keyed under the active project, so a
 * project or workspace switch (which drops that project's queries) reads it
 * afresh with the new identity.
 */
export function usePublicAgents(projectId: number | null, search: string, enabled: boolean) {
  const needle = search.trim();
  return useInfiniteQuery({
    queryKey: [...queryKeys.discover(projectId ?? 0), 'list', needle],
    enabled: enabled && projectId !== null,
    initialPageParam: 0,
    staleTime: 60_000,
    queryFn: ({ pageParam, signal }) => fetchPublicAgents(needle, pageParam, sessionApiClient(), signal),
    getNextPageParam: nextPublicAgentOffset,
  });
}

/**
 * The icon to load, or null. Only the workspace's own origin is accepted (the
 * server stores `/icons/<project>/<file>`, served without authentication):
 * an icon URL on another host would let a catalogue entry make the device
 * fetch from anywhere. No credential is ever attached to the request.
 */
export function publicAgentIconUrl(iconPath: string | null, origin: string | null): string | null {
  if (iconPath === null || origin === null) return null;
  let base: URL;
  try {
    base = new URL(origin);
  } catch {
    return null;
  }
  if (iconPath.startsWith('//')) return null;
  let url: URL;
  try {
    url = new URL(iconPath, base);
  } catch {
    return null;
  }
  if (url.origin !== base.origin || (url.protocol !== 'https:' && url.protocol !== 'http:')) return null;
  if (url.username !== '' || url.password !== '') return null;
  return url.toString();
}

export interface PublicAgentDetail {
  welcomeMessage: string | null;
  starters: string[];
}

const Detail = z.looseObject({
  version_details: z
    .looseObject({
      welcome_message: z.string().max(5000).nullish().catch(null),
      conversation_starters: z.array(z.unknown()).max(100).nullish().catch(null),
    })
    .nullish(),
});

export function publicAgentDetailOf(data: unknown): PublicAgentDetail | null {
  const parsed = Detail.safeParse(data);
  if (!parsed.success || !parsed.data.version_details) return null;
  const version = parsed.data.version_details;
  const starters: string[] = [];
  for (const raw of version.conversation_starters ?? []) {
    if (typeof raw !== 'string') continue;
    const text = raw.trim().slice(0, 500);
    if (text === '' || starters.includes(text)) continue;
    starters.push(text);
    if (starters.length >= MAX_STARTERS) break;
  }
  const welcome = version.welcome_message?.trim();
  return { welcomeMessage: welcome ? welcome : null, starters };
}

/** The published version's welcome message and starters; null when the server does not answer them. */
export async function fetchPublicAgentDetail(
  agent: Pick<PublicAgent, 'id' | 'versionName'>,
  client: ApiClient = sessionApiClient(),
  signal?: AbortSignal,
): Promise<PublicAgentDetail | null> {
  if (agent.versionName === '') return null;
  const { data, response } = await client.GET('/elitea_core/public_application/prompt_lib/{application_id}/{version_name}', {
    params: { path: { application_id: agent.id, version_name: agent.versionName } },
    signal,
  });
  if (!response.ok) return null;
  return publicAgentDetailOf(data);
}

export function usePublicAgentDetail(projectId: number | null, agent: PublicAgent | null) {
  return useQuery({
    queryKey: [...queryKeys.discover(projectId ?? 0), 'detail', agent?.id ?? 0, agent?.versionName ?? ''],
    enabled: projectId !== null && agent !== null,
    staleTime: 5 * 60_000,
    retry: false,
    queryFn: ({ signal }) => fetchPublicAgentDetail(agent as PublicAgent, sessionApiClient(), signal),
  });
}

/** Looks one agent up in the cached catalogue pages (the details screen is opened from the list). */
export function findCachedPublicAgent(
  pages: readonly (PublicAgentPage[] | undefined)[],
  agentId: number,
): PublicAgent | null {
  for (const list of pages) {
    for (const page of list ?? []) {
      const hit = page.agents.find((a) => a.id === agentId);
      if (hit) return hit;
    }
  }
  return null;
}

/**
 * The agent from the project's cached catalogue pages, kept current: the page
 * subscribes to the query cache, so a list refetch underneath it (app focus,
 * reconnect) or a list that arrives after the page opened (a project switch)
 * re-renders it with the row as it is now, and a chat pins the published
 * version the server answers today, not the one seen when the page opened.
 */
export function useCachedPublicAgent(projectId: number | null, agentId: number | null): PublicAgent | null {
  const queryClient = useQueryClient();
  const subscribe = useCallback((onChange: () => void) => queryClient.getQueryCache().subscribe(onChange), [queryClient]);
  const read = useCallback((): PublicAgent | null => {
    if (projectId === null || agentId === null) return null;
    const cached = queryClient
      .getQueriesData<InfiniteData<PublicAgentPage, number>>({ queryKey: queryKeys.discover(projectId) })
      .map(([, data]) => (data && Array.isArray(data.pages) ? data.pages : undefined));
    return findCachedPublicAgent(cached, agentId);
  }, [queryClient, projectId, agentId]);
  // Stable between changes: the hit is the row object held in the cache.
  return useSyncExternalStore(subscribe, read, read);
}

type CreateConversationBody = components['schemas']['ConversationCreateRequest'];

const Created = z.looseObject({ id: NumericId, uuid: z.string().min(1).max(100) });

/**
 * Why a start failed. The two steps fail for different reasons, so they are
 * named apart: `not_allowed` / `project_unavailable` come from creating the
 * conversation in the ACTIVE project; `not_available` / `not_shared` from
 * adding the public agent to it.
 */
export type StartPublicChatFailure = 'not_allowed' | 'project_unavailable' | 'not_available' | 'not_shared' | 'failed';

export class StartPublicChatError extends Error {
  constructor(
    readonly reason: StartPublicChatFailure,
    readonly status?: number,
    /**
     * A conversation was created but could not be deleted again (the link
     * dropped, or the server refused the DELETE): an empty, agent-less
     * conversation may be in the list. The caller refreshes the list.
     */
    readonly leftover: boolean = false,
  ) {
    super(`could not start a chat with a public agent: ${reason}`);
    this.name = 'StartPublicChatError';
  }
}

export function startPublicChatErrorText(error: unknown): string {
  if (error instanceof StartPublicChatError) {
    if (error.leftover) return "Couldn't start the conversation. An empty one may be left in your list.";
    if (error.reason === 'not_allowed') return "You can't start conversations in this project.";
    if (error.reason === 'project_unavailable') return "This project isn't available any more. Pick another project and try again.";
    if (error.reason === 'not_available') return 'This agent is no longer available.';
    if (error.reason === 'not_shared') return "This agent isn't shared with you.";
  }
  return "Couldn't start the conversation. Try again.";
}

/** createConversation in the active project: a refusal or a missing project is about the project. */
function createFailureOf(status: number | undefined): StartPublicChatError {
  if (status === 403) return new StartPublicChatError('not_allowed', status);
  if (status === 404 || status === 410) return new StartPublicChatError('project_unavailable', status);
  return new StartPublicChatError('failed', status);
}

/** addConversationParticipants with the public agent: a refusal or a missing version is about the agent. */
function addFailureOf(status: number | undefined): StartPublicChatFailure {
  if (status === 403) return 'not_shared';
  if (status === 400 || status === 404 || status === 410 || status === 422) return 'not_available';
  return 'failed';
}

/**
 * Starts a private conversation in `projectId` (the active project) with a
 * public agent, pinned to its published version. When the agent cannot be
 * added, the empty conversation just created is deleted again. That DELETE
 * can fail as well (most often the same dropped link that failed the add):
 * the error then carries `leftover`, so the caller refreshes the list and
 * says an empty conversation may be in it.
 */
export async function startConversationWithPublicAgent(
  projectId: number,
  agent: Pick<PublicAgent, 'id' | 'publicProjectId' | 'name' | 'versionId'>,
  client: ApiClient = sessionApiClient(),
): Promise<{ id: number; uuid: string }> {
  const body: CreateConversationBody = { name: agent.name.slice(0, 200), is_private: true, participants: [] };
  const create = await client.POST('/elitea_core/conversations/prompt_lib/{project_id}', {
    params: { path: { project_id: String(projectId) } },
    body,
  });
  if (!create.response.ok) throw createFailureOf(create.response.status);
  const created = Created.safeParse(create.data);
  if (!created.success) throw new StartPublicChatError('failed');
  try {
    const rows = await addParticipants(
      projectId,
      created.data.uuid,
      [
        {
          entity_name: 'application',
          entity_meta: { id: agent.id, project_id: agent.publicProjectId, name: agent.name },
          entity_settings: { version_id: agent.versionId },
        },
      ],
      client,
    );
    if (rows.length === 0) throw new StartPublicChatError('failed');
  } catch (e) {
    const removed = await client
      .DELETE('/elitea_core/conversation/prompt_lib/{project_id}/{conversation_id}', {
        params: { path: { project_id: String(projectId), conversation_id: created.data.uuid } },
      })
      .then(
        ({ response }) => response.ok,
        () => false,
      );
    const leftover = !removed;
    const status = (e as { status?: number }).status;
    if (e instanceof StartPublicChatError) throw new StartPublicChatError(e.reason, e.status, leftover);
    // addParticipants' own error: the agent's version is gone (400/404/422) or not shared with the caller (403).
    throw new StartPublicChatError(addFailureOf(status), status, leftover);
  }
  return { id: created.data.id, uuid: created.data.uuid };
}
