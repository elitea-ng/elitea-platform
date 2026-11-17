import { useQuery } from '@tanstack/react-query';
import { z } from 'zod';

import { sessionApiClient, type ApiClient } from '@/api/client';
import type { components } from '@/api/generated/elitea.gen';
import { queryKeys } from '@/api/queries/keys';

/**
 * The skills pinned to a conversation (client contract 1.57, ADR-0050 amendment 1):
 * listConversationSkills, pinConversationSkill and unpinConversationSkill, and listSkills for the
 * skills a person can add. A pin applies to every run of the conversation whichever agent or
 * model answers, and is shown in its own section of the conversation sheet, never among the
 * participants. It carries no skill text: the server reads the text when each turn starts. Online
 * only; a 404, 426 or 501 reads as "this workspace has none", a 403 as "not allowed".
 */

export type ConversationSkillPin = components['schemas']['ConversationSkillPin'];

/** How many skills one conversation may hold: the five a turn may invoke. */
export const CONVERSATION_SKILL_LIMIT = 5;

export type SkillPinFailure = 'not_allowed' | 'not_available' | 'unavailable_skill' | 'limit' | 'failed';

export class SkillPinError extends Error {
  constructor(readonly reason: SkillPinFailure, readonly status?: number) {
    super(`conversation skills: ${reason}`);
    this.name = 'SkillPinError';
  }
}

export function skillPinErrorText(error: unknown): string {
  if (!(error instanceof SkillPinError)) return "That didn't work. Try again.";
  switch (error.reason) {
    case 'not_allowed':
      return "You can't change the skills of this conversation.";
    case 'not_available':
      return 'This workspace does not support that from mobile yet.';
    case 'unavailable_skill':
      return 'That skill is not available to you.';
    case 'limit':
      return 'A conversation holds at most five skills. Remove one to add another.';
    default:
      return "That didn't work. Try again.";
  }
}

function failureOf(status: number): SkillPinError {
  if (status === 403) return new SkillPinError('not_allowed', status);
  if (status === 404) return new SkillPinError('unavailable_skill', status);
  if (status === 409) return new SkillPinError('limit', status);
  if (status === 426 || status === 501) return new SkillPinError('not_available', status);
  return new SkillPinError('failed', status);
}

const RowId = z.number().int().positive();
const SkillId = z.number().int().positive().max(2147483647);

/** The pins of a conversation, oldest first; null when the workspace has no conversation skills (before 1.57). */
export async function fetchConversationSkills(
  projectId: number,
  conversationId: string,
  client: ApiClient = sessionApiClient(),
  signal?: AbortSignal,
): Promise<ConversationSkillPin[] | null> {
  const { data, response } = await client.GET('/elitea_core/conversation_skills/prompt_lib/{project_id}/{conversation_id}', {
    params: { path: { project_id: String(projectId), conversation_id: conversationId } },
    signal,
  });
  if (!response.ok) {
    if (response.status === 404 || response.status === 403 || response.status === 426 || response.status === 501) return null;
    throw failureOf(response.status);
  }
  return (Array.isArray(data?.rows) ? data.rows : []).filter((row) => RowId.safeParse(row?.id).success);
}

export function useConversationSkills(projectId: number | null, conversationId: string | null, enabled: boolean) {
  return useQuery({
    queryKey: [...queryKeys.projectRoot(projectId ?? 0), 'conversation-skills', conversationId ?? ''],
    enabled: enabled && projectId !== null && conversationId !== null,
    retry: false,
    queryFn: ({ signal }) => fetchConversationSkills(projectId as number, conversationId as string, sessionApiClient(), signal),
  });
}

/** Pins a project skill. Pinning what is pinned already answers the existing pin. */
export async function pinConversationSkill(
  projectId: number,
  conversationId: string,
  skillId: number,
  client: ApiClient = sessionApiClient(),
): Promise<ConversationSkillPin> {
  if (!SkillId.safeParse(skillId).success) throw new SkillPinError('failed');
  const { data, response } = await client.POST('/elitea_core/conversation_skills/prompt_lib/{project_id}/{conversation_id}', {
    params: { path: { project_id: String(projectId), conversation_id: conversationId } },
    body: { skill_id: skillId },
  });
  if (!response.ok || !data) throw failureOf(response.status);
  return data;
}

export async function unpinConversationSkill(
  projectId: number,
  conversationId: string,
  pinId: number,
  client: ApiClient = sessionApiClient(),
): Promise<void> {
  if (!RowId.safeParse(pinId).success) throw new SkillPinError('failed');
  const { response } = await client.DELETE('/elitea_core/conversation_skill/prompt_lib/{project_id}/{conversation_id}/{pin_id}', {
    params: { path: { project_id: String(projectId), conversation_id: conversationId, pin_id: pinId } },
  });
  if (!response.ok && response.status !== 404) throw failureOf(response.status);
}

export interface ProjectSkillOption {
  id: number;
  name: string;
  description: string;
}

/** One page of the project's skills (the server's largest), the ones a person can pin. */
export async function fetchProjectSkills(
  projectId: number,
  client: ApiClient = sessionApiClient(),
  signal?: AbortSignal,
): Promise<ProjectSkillOption[]> {
  const { data, response } = await client.GET('/elitea_core/skills/prompt_lib/{project_id}', {
    params: { path: { project_id: String(projectId) }, query: { page: 1, page_size: 100 } },
    signal,
  });
  if (!response.ok) throw failureOf(response.status);
  const rows = Array.isArray(data?.items) ? data.items : [];
  return rows
    .flatMap((row): ProjectSkillOption[] => {
      const id = Number(row.id);
      return SkillId.safeParse(id).success && typeof row.name === 'string' && row.name.trim() !== ''
        ? [{ id, name: row.name.trim(), description: typeof row.description === 'string' ? row.description.trim().slice(0, 200) : '' }]
        : [];
    })
    .sort((left, right) => left.name.localeCompare(right.name));
}

export function useProjectSkills(projectId: number | null, enabled: boolean) {
  return useQuery({
    queryKey: [...queryKeys.projectRoot(projectId ?? 0), 'skill-options'],
    enabled: enabled && projectId !== null,
    retry: false,
    staleTime: 30_000,
    queryFn: ({ signal }) => fetchProjectSkills(projectId as number, sessionApiClient(), signal),
  });
}
