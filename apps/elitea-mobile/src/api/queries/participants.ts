import { useQuery } from '@tanstack/react-query';
import { z } from 'zod';

import { sessionApiClient, type ApiClient } from '@/api/client';
import type { components } from '@/api/generated/elitea.gen';
import { queryKeys } from '@/api/queries/keys';
import { fetchAgentVersion } from '@/api/queries/agents';

/**
 * Conversation participants from mobile (client contract 1.1):
 * addConversationParticipants, deleteConversationParticipant and
 * listParticipantCandidates. Online only; a 404 or 501 from an older server
 * reads as "not available", a 403 as "not allowed".
 */

export type Participant = components['schemas']['ConversationParticipant'];
type ParticipantCreate = components['schemas']['ConversationParticipantCreate'];

export type ParticipantFailure = 'not_allowed' | 'not_available' | 'no_version' | 'invalid' | 'failed';

export class ParticipantError extends Error {
  constructor(readonly reason: ParticipantFailure, readonly status?: number) {
    super(`participants: ${reason}`);
    this.name = 'ParticipantError';
  }
}

export function participantErrorText(error: unknown): string {
  if (!(error instanceof ParticipantError)) return "That didn't work. Try again.";
  switch (error.reason) {
    case 'not_allowed':
      return "You can't change who is in this conversation.";
    case 'not_available':
      return 'This workspace does not support that from mobile yet.';
    case 'no_version':
      return 'This agent has no version to run yet.';
    case 'invalid':
      return 'The workspace refused that participant.';
    default:
      return "That didn't work. Try again.";
  }
}

function failureOf(status: number): ParticipantError {
  if (status === 403) return new ParticipantError('not_allowed', status);
  if (status === 404 || status === 501) return new ParticipantError('not_available', status);
  if (status === 400 || status === 422) return new ParticipantError('invalid', status);
  return new ParticipantError('failed', status);
}

/** The participant row id must be a positive integer before it goes in a path. */
const RowId = z.number().int().positive();

/** Adds participants; answers the rows of this request (an existing one comes back as is). */
export async function addParticipants(
  projectId: number,
  conversationId: string,
  items: ParticipantCreate[],
  client: ApiClient = sessionApiClient(),
): Promise<Participant[]> {
  if (items.length === 0 || items.length > 100) throw new ParticipantError('invalid');
  const { data, response } = await client.POST('/elitea_core/participants/prompt_lib/{project_id}/{conversation_id}', {
    params: { path: { project_id: String(projectId), conversation_id: conversationId } },
    body: items,
  });
  if (!response.ok) throw failureOf(response.status);
  return Array.isArray(data) ? data : [];
}

export interface AgentRef {
  id: number;
  name: string;
  /** The agent's project; defaults to the conversation's. */
  projectId?: number;
}

/**
 * Adds an agent pinned to its current version (a participant without
 * `version_id` cannot run) and answers its participant row.
 */
export async function addAgentParticipant(
  projectId: number,
  conversationId: string,
  agent: AgentRef,
  client: ApiClient = sessionApiClient(),
): Promise<Participant> {
  const agentProject = agent.projectId ?? projectId;
  const version = await fetchAgentVersion(agentProject, agent.id, client).catch(() => null);
  if (version === null) throw new ParticipantError('no_version');
  const [row] = await addParticipants(
    projectId,
    conversationId,
    [
      {
        entity_name: 'application',
        entity_meta: { id: agent.id, project_id: agentProject, name: agent.name },
        entity_settings: { version_id: version.versionId },
      },
    ],
    client,
  );
  if (!row || !RowId.safeParse(row.id).success) throw new ParticipantError('failed');
  return row;
}

/** Adds a person (a project member, by USER id). */
export async function addPersonParticipant(
  projectId: number,
  conversationId: string,
  person: { userId: number; name: string },
  client: ApiClient = sessionApiClient(),
): Promise<Participant> {
  if (!Number.isSafeInteger(person.userId) || person.userId <= 0) throw new ParticipantError('invalid');
  const [row] = await addParticipants(
    projectId,
    conversationId,
    [{ entity_name: 'user', entity_meta: { id: person.userId, user_name: person.name } }],
    client,
  );
  if (!row) throw new ParticipantError('failed');
  return row;
}

export async function removeParticipant(
  projectId: number,
  conversationId: string,
  participantId: number,
  client: ApiClient = sessionApiClient(),
): Promise<void> {
  if (!RowId.safeParse(participantId).success) throw new ParticipantError('invalid');
  const { response } = await client.DELETE('/elitea_core/participant/prompt_lib/{project_id}/{conversation_id}/{participant_id}', {
    params: { path: { project_id: String(projectId), conversation_id: conversationId, participant_id: participantId } },
  });
  if (!response.ok && response.status !== 404) throw failureOf(response.status);
}

export interface CandidatePage {
  rows: { userId: number; name: string; email: string | null; alreadyParticipant: boolean }[];
  nextCursor: string | null;
}

/**
 * People who can be added (project members), filtered by `q` on the server.
 * Null when the server does not offer the list (older than 1.1): the sheet
 * then hides "Add people" rather than fall back to an admin list.
 */
export async function fetchParticipantCandidates(
  projectId: number,
  conversationId: string,
  q: string,
  cursor: string | null = null,
  client: ApiClient = sessionApiClient(),
  signal?: AbortSignal,
): Promise<CandidatePage | null> {
  const { data, response } = await client.GET('/elitea_core/participant_candidates/prompt_lib/{project_id}/{conversation_id}', {
    params: {
      path: { project_id: String(projectId), conversation_id: conversationId },
      query: { type: 'user', limit: 20, ...(q.trim() ? { q: q.trim().slice(0, 100) } : {}), ...(cursor ? { cursor } : {}) },
    },
    signal,
  });
  if (!response.ok) {
    if (response.status === 404 || response.status === 501 || response.status === 403) return null;
    throw failureOf(response.status);
  }
  const rows = (Array.isArray(data?.rows) ? data.rows : [])
    .filter((r) => Number.isSafeInteger(r?.user_id) && r.user_id > 0)
    .map((r) => ({
      userId: r.user_id,
      name: typeof r.name === 'string' && r.name.trim() !== '' ? r.name.trim() : `User ${r.user_id}`,
      email: typeof r.email === 'string' && r.email !== '' ? r.email : null,
      alreadyParticipant: r.already_participant === true,
    }));
  return { rows, nextCursor: typeof data?.next_cursor === 'string' ? data.next_cursor : null };
}

export function useParticipantCandidates(projectId: number | null, conversationId: string | null, q: string, enabled: boolean) {
  return useQuery({
    queryKey: [...queryKeys.projectRoot(projectId ?? 0), 'candidates', conversationId ?? '', q.trim()],
    enabled: enabled && projectId !== null && conversationId !== null,
    retry: false,
    queryFn: ({ signal }) => fetchParticipantCandidates(projectId as number, conversationId as string, q, null, sessionApiClient(), signal),
  });
}
