/**
 * **Try** a skill (ADR-0051 amendment 1, A1.3): a new conversation with that one skill pinned, opened in the chat.
 *
 * It is a plain conversation: the skill is a conversation skill (ADR-0050 amendment 1), applied to whichever model
 * or agent answers, through the worker's own skill loading. There is no skill runner. A skill from a listing that is
 * not in the project is pinned by reference, so nothing is forked; it is untrusted, and the chat's Skills section
 * says so. The conversation carries `meta.origin = "skill_try"`.
 */
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { useNavigate } from '@tanstack/react-router';

import { conversationSkillKeys } from '@/entities/conversation-skill';
import { pinConversationSkill } from '@/shared/api/generated/chat/chat';
import { eliteaFetch } from '@/shared/api/generated/mutator';
import type { PinConversationSkillRequest } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { useSelectedProjectId } from '@/pages/skills/lib/useSelectedProjectId';

const JSON_HEADERS = { 'Content-Type': 'application/json' } as const;

export interface TrySkillTarget {
  readonly name: string;
  /** What to pin: a project skill, or a listing version by reference. */
  readonly pin: PinConversationSkillRequest;
}

export interface TrySkill {
  /** Starts the conversation; undefined while no project is selected. */
  readonly start: (() => void) | undefined;
  readonly isStarting: boolean;
  readonly isError: boolean;
}

export function useTrySkill(target: TrySkillTarget): TrySkill {
  const projectId = useSelectedProjectId();
  const navigate = useNavigate();
  const queryClient = useQueryClient();

  const mutation = useMutation({
    mutationFn: async (project: string): Promise<string> => {
      const created = await eliteaFetch<{ data: { readonly id?: string | number } }>(
        `/elitea_core/conversations/prompt_lib/${project}`,
        {
          method: 'POST',
          body: JSON.stringify({
            name: t('skills.try.name', 'Try: {{name}}', { name: target.name }),
            participants: [],
            meta: { origin: 'skill_try' },
          }),
          headers: JSON_HEADERS,
        },
      );
      const conversationId = created.data.id;
      if (conversationId === undefined) throw new Error('The conversation was not created');
      try {
        await pinConversationSkill(project, String(conversationId), target.pin);
      } catch (error) {
        // The listing may have been yanked since the page loaded: do not leave an empty "Try" conversation behind.
        await eliteaFetch(`/elitea_core/conversation/prompt_lib/${project}/${String(conversationId)}`, { method: 'DELETE' }).catch(() => undefined);
        throw error;
      }
      return String(conversationId);
    },
    onSuccess: (conversationId) => {
      void queryClient.invalidateQueries({ queryKey: ['folder', 'list'] });
      if (projectId !== undefined) void queryClient.invalidateQueries({ queryKey: conversationSkillKeys.all(projectId) });
      void navigate({ to: '/chat/$conversationId', params: { conversationId } });
    },
  });

  return {
    start: projectId === undefined ? undefined : () => mutation.mutate(projectId),
    isStarting: mutation.isPending,
    isError: mutation.isError,
  };
}
