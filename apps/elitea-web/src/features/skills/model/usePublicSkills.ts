/**
 * The public skill catalog's reads, and the attach that consumes from it.
 *
 * The catalog is a project-independent surface — `GET /public_skills/prompt_lib`
 * takes no project id, because the catalog IS the public project — but the
 * ATTACH is not: it forks the published skill into the caller's own project
 * before mapping it onto agent versions, so it takes one.
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import {
  attachPublicSkill,
  fetchAgentsWithSkill,
  fetchPublicSkills,
  type PublicSkillQuery,
} from '../api/skillPublishApi';
import { skillQueryKeys } from './useSkills';

const publicSkillQueryKeys = {
  list: (request: PublicSkillQuery) =>
    ['public-skills', request.query ?? '', request.category ?? ''] as const,
  agentsWithSkill: (projectId: string | undefined, publicSkillId: number | undefined) =>
    ['public-skills', 'agents-with-skill', projectId ?? '', publicSkillId ?? ''] as const,
};

export function usePublicSkills(request: PublicSkillQuery) {
  return useQuery({
    queryKey: publicSkillQueryKeys.list(request),
    queryFn: () => fetchPublicSkills(request),
  });
}

/**
 * The agent versions in this project that already carry this public skill —
 * the attach dialog's "already added" state (issue #625 item 3). Disabled
 * until both ids are known, so the dialog does not fire a request for
 * `undefined`/`skill?.id` while it is still resolving which skill was opened.
 */
export function useAgentsWithSkill(projectId: string | undefined, publicSkillId: number | undefined, enabled = true) {
  return useQuery({
    queryKey: publicSkillQueryKeys.agentsWithSkill(projectId, publicSkillId),
    queryFn: () => fetchAgentsWithSkill(projectId ?? '', publicSkillId),
    enabled: enabled && projectId !== undefined && publicSkillId !== undefined,
  });
}

export interface AttachSkillArgs {
  readonly publicSkillId: number;
  readonly publicVersionId: number;
  readonly agentVersionIds: readonly number[];
}

/**
 * Attaches a published skill to one or more agent versions.
 *
 * The route answers 200 with a per-agent `results` list even when some of the
 * attachments failed, so the caller is handed those outcomes rather than a
 * boolean: reporting "attached" off the HTTP status alone would report success
 * for an attach that attached nothing.
 *
 * On success the caller's own skill list is invalidated, because the fork the
 * attach performs puts a NEW skill in their project and a stale list would not
 * show it.
 */
export function useAttachPublicSkill(projectId: string | undefined) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (args: AttachSkillArgs) => attachPublicSkill(projectId ?? '', args),
    onSuccess: async () => {
      if (projectId) await queryClient.invalidateQueries({ queryKey: skillQueryKeys.all(projectId) });
    },
  });
}
