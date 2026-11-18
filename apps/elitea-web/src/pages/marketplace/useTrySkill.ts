/**
 * **Try** a skill (ADR-0051 amendment 1, A1.3): a new conversation with that one skill pinned, opened in the chat.
 *
 * It is a plain conversation: the skill is a conversation skill (ADR-0050 amendment 1), applied to whichever model
 * or agent answers, through the worker's own skill loading. There is no skill runner. A skill from a listing that is
 * not in the project is pinned by reference, so nothing is forked; it is untrusted, and the chat's Skills section
 * says so. The conversation carries `meta.origin = "skill_try"`.
 *
 * It opens with the project's default model as its one participant and no tools (A1.3), so the first message just
 * works. The default is resolved as the new-chat screen resolves it: the model marked `default`, else the first the
 * project can run. A project with no model still gets the conversation, without a participant.
 */
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { useNavigate } from '@tanstack/react-router';

import { conversationSkillKeys } from '@/entities/conversation-skill';
import { listModels, type ConfigModel } from '@/shared/api/configurationsApi';
import { pinConversationSkill } from '@/shared/api/generated/chat/chat';
import { eliteaFetch } from '@/shared/api/generated/mutator';
import type { PinConversationSkillRequest } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { ChatParticipantType } from '@/shared/lib/chat';
import { useSelectedProjectId } from '@/pages/skills/lib/useSelectedProjectId';

const JSON_HEADERS = { 'Content-Type': 'application/json' } as const;

interface TrySkillTarget {
  readonly name: string;
  /** What to pin: a project skill, or a listing version by reference. */
  readonly pin: PinConversationSkillRequest;
  /** `try` (the default) is the labelled preview; `use` is a plain new chat that carries the skill (A1.4). */
  readonly kind?: 'try' | 'use';
}

export interface TrySkill {
  /** Starts the conversation for one skill; undefined while no project is selected. */
  readonly start: ((target: TrySkillTarget) => void) | undefined;
  readonly isStarting: boolean;
  readonly isError: boolean;
}

/** The model the new-chat screen starts on: the one marked default, else the first. */
export function resolveDefaultModel(items: readonly ConfigModel[]): ConfigModel | undefined {
  return items.find((model) => model.default === true) ?? items[0];
}

/** The model participant of a Try: the default model alone, with the project it lives in (a shared one is not the chat's). */
export function modelParticipant(model: ConfigModel): Record<string, unknown> {
  return {
    entity_name: ChatParticipantType.Models,
    entity_meta: { model_name: model.name },
    entity_settings: { model_project_id: Number(model.project_id) },
  };
}

/** The default model's participant, or none when the project has no model or the catalogue cannot be read: Try still opens. */
async function defaultParticipants(project: string): Promise<readonly Record<string, unknown>[]> {
  try {
    const model = resolveDefaultModel((await listModels({ projectId: project, include_shared: true })).items);
    return model === undefined ? [] : [modelParticipant(model)];
  } catch {
    return [];
  }
}

/** Every entry point of a skill shares this one starter; each `start` names its own skill. */
export function useTrySkill(): TrySkill {
  const projectId = useSelectedProjectId();
  const navigate = useNavigate();
  const queryClient = useQueryClient();

  const mutation = useMutation({
    mutationFn: async ({ project, target }: { readonly project: string; readonly target: TrySkillTarget }): Promise<string> => {
      const created = await eliteaFetch<{ data: { readonly id?: string | number } }>(
        `/elitea_core/conversations/prompt_lib/${project}`,
        {
          method: 'POST',
          body: JSON.stringify({
            name: target.kind === 'use' ? target.name : t('skills.try.name', 'Try: {{name}}', { name: target.name }),
            participants: await defaultParticipants(project),
            ...(target.kind === 'use' ? {} : { meta: { origin: 'skill_try' } }),
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
    start: projectId === undefined ? undefined : (target) => mutation.mutate({ project: projectId, target }),
    isStarting: mutation.isPending,
    isError: mutation.isError,
  };
}
