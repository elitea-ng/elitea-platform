import { useQuery } from '@tanstack/react-query';
import { z } from 'zod';

import { sessionApiClient, type ApiClient } from '@/api/client';
import { unwrap } from '@/api/errors';
import type { components } from '@/api/generated/elitea.gen';
import { queryKeys } from '@/api/queries/keys';

/**
 * Agents (applications) a new conversation can be started with, and the
 * create call. Application and version ids arrive as numbers or numeric
 * strings depending on the route, so they are normalised at this boundary.
 */

const PositiveId = z
  .union([z.number(), z.string().regex(/^\d{1,15}$/)])
  .transform(Number)
  .refine((n) => Number.isSafeInteger(n) && n > 0);

const AgentRow = z.looseObject({
  id: PositiveId,
  name: z.string().max(500).nullish(),
  description: z.string().nullish(),
  agent_type: z.string().nullish(),
});

export interface Agent {
  id: number;
  name: string;
  description: string;
}

export const AGENTS_PAGE_SIZE = 50;

export async function fetchAgents(
  projectId: number,
  search: string = '',
  client: ApiClient = sessionApiClient(),
  signal?: AbortSignal,
): Promise<Agent[]> {
  const page = await unwrap(
    client.GET('/elitea_core/applications/prompt_lib/{project_id}', {
      params: {
        path: { project_id: String(projectId) },
        query: { limit: AGENTS_PAGE_SIZE, offset: 0, ...(search.trim() ? { query: search.trim() } : {}) },
      },
      signal,
    }),
  );
  const rows: unknown[] = Array.isArray(page?.rows) ? page.rows : [];
  const agents: Agent[] = [];
  for (const raw of rows) {
    const parsed = AgentRow.safeParse(raw);
    if (!parsed.success) continue;
    agents.push({
      id: parsed.data.id,
      name: parsed.data.name?.trim() || `Agent ${parsed.data.id}`,
      description: parsed.data.description?.trim() ?? '',
    });
  }
  return agents;
}

export function useAgents(projectId: number | null, search: string = '') {
  return useQuery({
    queryKey: [...queryKeys.agents(projectId ?? 0), search],
    enabled: projectId !== null,
    queryFn: ({ signal }) => fetchAgents(projectId as number, search, sessionApiClient(), signal),
  });
}

const AgentDetail = z.looseObject({
  version_details: z
    .looseObject({
      id: PositiveId,
      meta: z.looseObject({ internal_tools: z.array(z.string()).nullish() }).nullish().catch(null),
    })
    .nullish(),
});

/** What a conversation needs to know about an agent's current version. */
export interface AgentVersion {
  /** The version a participant runs (`entity_settings.version_id`). */
  versionId: number;
  /** The version's enabled internal tools (`attachments`, `ask_user`, …). */
  internalTools: string[];
}

/** The agent's current (default) version, or null when it has none. */
export async function fetchAgentVersion(
  projectId: number,
  agentId: number,
  client: ApiClient = sessionApiClient(),
  signal?: AbortSignal,
): Promise<AgentVersion | null> {
  const detail = await unwrap(
    client.GET('/elitea_core/application/prompt_lib/{project_id}/{application_id}', {
      params: { path: { project_id: String(projectId), application_id: agentId } },
      signal,
    }),
  );
  const parsed = AgentDetail.safeParse(detail);
  const version = parsed.success ? parsed.data.version_details : null;
  if (!version) return null;
  return { versionId: version.id, internalTools: (version.meta?.internal_tools ?? []).filter((t) => typeof t === 'string') };
}

/** The version a new conversation addresses: the agent's current (default) version. */
export async function fetchAgentVersionId(
  projectId: number,
  agentId: number,
  client: ApiClient = sessionApiClient(),
): Promise<number | null> {
  return (await fetchAgentVersion(projectId, agentId, client))?.versionId ?? null;
}

export function useAgentVersion(projectId: number | null, agentId: number | null) {
  return useQuery({
    queryKey: [...queryKeys.agents(projectId ?? 0), 'version', agentId ?? 0],
    enabled: projectId !== null && agentId !== null,
    staleTime: 60_000,
    queryFn: ({ signal }) => fetchAgentVersion(projectId as number, agentId as number, sessionApiClient(), signal),
  });
}

/**
 * The create body. Upstream (06a490427) types `participants` with the READ
 * shape (`ConversationParticipant`, `id` required) although the create route
 * reads the same keys as `addConversationParticipants`
 * (`ConversationParticipantCreate`, no id); the app sends the create shape.
 */
type CreateConversationRequest = Omit<components['schemas']['ConversationCreateRequest'], 'participants'> & {
  participants: components['schemas']['ConversationParticipantCreate'][];
};
type WireCreateConversationRequest = components['schemas']['ConversationCreateRequest'];

const Created = z.looseObject({
  id: PositiveId,
  uuid: z.string().nullish(),
});

export class StartConversationError extends Error {
  constructor(readonly reason: 'no_version' | 'bad_response') {
    super(`could not start a conversation: ${reason}`);
    this.name = 'StartConversationError';
  }
}

/**
 * Creates a private conversation with `agent` as its participant, pinned to
 * the agent's current version (a participant without `version_id` cannot be
 * executed). Returns the new conversation's id (route reference).
 */
export async function startConversationWithAgent(
  projectId: number,
  agent: Agent,
  client: ApiClient = sessionApiClient(),
): Promise<{ id: number; uuid: string | null }> {
  const versionId = await fetchAgentVersionId(projectId, agent.id, client);
  if (versionId === null) throw new StartConversationError('no_version');
  const body: CreateConversationRequest = {
    name: agent.name.slice(0, 200),
    is_private: true,
    participants: [
      {
        entity_name: 'application',
        entity_meta: { id: agent.id, project_id: projectId },
        entity_settings: { version_id: versionId },
      },
    ],
  };
  const created = await unwrap(
    client.POST('/elitea_core/conversations/prompt_lib/{project_id}', {
      params: { path: { project_id: String(projectId) } },
      body: body as unknown as WireCreateConversationRequest,
    }),
  );
  const parsed = Created.safeParse(created);
  if (!parsed.success) throw new StartConversationError('bad_response');
  return { id: parsed.data.id, uuid: parsed.data.uuid ?? null };
}
