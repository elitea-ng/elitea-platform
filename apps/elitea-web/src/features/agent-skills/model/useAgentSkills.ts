import { useMutation, useQuery, useQueryClient, type UseMutationResult } from '@tanstack/react-query';

import {
  attachSkill,
  detachSkill,
  fetchAttachedSkills,
  fetchProjectSkills,
  MAX_SKILLS_PER_AGENT,
  type AgentSkill,
} from '../api/agentSkillsApi';

/**
 * The agent editor's SKILLS section, as data.
 *
 * The attached list and the picker list are two DIFFERENT queries against two
 * different routes, and they are kept apart on purpose. Reusing one key for
 * both is how the picker ends up showing the attached five and the section
 * ends up showing every skill in the project — which is precisely the defect
 * #367 fixed on the server side, in the other direction.
 *
 * Both mutations invalidate the ATTACHED key only. The project skills list is
 * unchanged by an attachment: attaching does not create or delete a skill.
 */

export const agentSkillKeys = {
  attached: (projectId: string, appVersionId: number) =>
    [`/elitea_core/application_skills/prompt_lib/${projectId}/${String(appVersionId)}`] as const,
  picker: (projectId: string, query: string) => ['agent-skills-picker', projectId, query] as const,
};

interface AttachVariables {
  readonly skillId: number;
  readonly skillVersionId: number;
}

export interface AgentSkillsState {
  readonly attached: readonly AgentSkill[];
  readonly isLoading: boolean;
  readonly isError: boolean;
  readonly max: number;
  readonly isFull: boolean;
  readonly attach: UseMutationResult<unknown, Error, AttachVariables>;
  readonly detach: UseMutationResult<void, Error, number>;
}

export function useAgentSkills(projectId: string | undefined, appVersionId: number | undefined): AgentSkillsState {
  const queryClient = useQueryClient();
  const enabled = projectId !== undefined && appVersionId !== undefined && Number.isFinite(appVersionId);

  const attachedQuery = useQuery({
    queryKey: agentSkillKeys.attached(projectId ?? '', appVersionId ?? 0),
    queryFn: () => fetchAttachedSkills(projectId ?? '', appVersionId ?? 0),
    enabled,
  });

  const invalidate = (): void => {
    void queryClient.invalidateQueries({ queryKey: agentSkillKeys.attached(projectId ?? '', appVersionId ?? 0) });
  };

  const attach = useMutation<unknown, Error, AttachVariables>({
    mutationFn: (variables) =>
      attachSkill(projectId ?? '', variables.skillId, appVersionId ?? 0, variables.skillVersionId),
    onSuccess: invalidate,
  });

  const detach = useMutation<void, Error, number>({
    mutationFn: (skillId) => detachSkill(projectId ?? '', skillId, appVersionId ?? 0),
    onSuccess: invalidate,
  });

  const attached = attachedQuery.data ?? [];
  return {
    attached,
    isLoading: attachedQuery.isLoading,
    isError: attachedQuery.isError,
    max: MAX_SKILLS_PER_AGENT,
    isFull: attached.length >= MAX_SKILLS_PER_AGENT,
    attach,
    detach,
  };
}

export interface SkillPickerState {
  readonly options: readonly AgentSkill[];
  readonly isLoading: boolean;
}

/**
 * The "+ Skill" picker's own list.
 *
 * `enabled` is the menu's open state, so the request runs when the menu opens
 * and not on every editor render — which is what production does (the list
 * request appears in the network log at the moment the menu opens, not before).
 */
export function useSkillPicker(projectId: string | undefined, query: string, enabled: boolean): SkillPickerState {
  const pickerQuery = useQuery({
    queryKey: agentSkillKeys.picker(projectId ?? '', query),
    queryFn: () => fetchProjectSkills(projectId ?? '', query),
    enabled: enabled && projectId !== undefined,
  });
  return { options: pickerQuery.data ?? [], isLoading: pickerQuery.isLoading };
}
