/**
 * **Try in chat** (the old Start conversation): a new conversation in the
 * selected project whose participant is the published copy of the agent, then
 * the chat page on it.
 *
 * Two calls, the same pair the toolkit test chat makes: create the
 * conversation with no participants, then add the agent. The route takes the
 * participant ARRAY as the whole body; a `{participants: [...]}` wrapper reads
 * as one participant with no `entity_meta` and is refused with 400. The agent
 * is the PUBLIC project's twin at its published version, so `project_id` is the
 * row's (the public project) and `version_id` pins that version.
 */
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { useNavigate } from '@tanstack/react-router';

import { eliteaFetch } from '@/shared/api/generated/mutator';
import { ChatParticipantType } from '@/shared/lib/chat';
import { useSelectedProjectId } from '@/pages/skills/lib/useSelectedProjectId';

import type { ApplicationData } from './types';

const JSON_HEADERS = { 'Content-Type': 'application/json' } as const;

interface CreatedConversationBody {
  readonly id?: string | number;
}

export interface TryInChat {
  /** Starts the conversation; undefined while no project is selected. */
  readonly start: (() => void) | undefined;
  readonly isStarting: boolean;
  readonly isError: boolean;
}

export function useTryInChat(app: ApplicationData): TryInChat {
  const projectId = useSelectedProjectId();
  const navigate = useNavigate();
  const queryClient = useQueryClient();

  const mutation = useMutation({
    mutationFn: async (project: string): Promise<string> => {
      const created = await eliteaFetch<{ data: CreatedConversationBody }>(`/elitea_core/conversations/prompt_lib/${project}`, {
        method: 'POST',
        body: JSON.stringify({ name: app.name, participants: [] }),
        headers: JSON_HEADERS,
      });
      const conversationId = created.data.id;
      if (conversationId === undefined) throw new Error('The conversation was not created');
      await eliteaFetch(`/elitea_core/participants/prompt_lib/${project}/${String(conversationId)}`, {
        method: 'POST',
        body: JSON.stringify([
          {
            entity_name: ChatParticipantType.Applications,
            entity_meta: { id: app.id, name: app.name, project_id: app.project_id },
            entity_settings: { version_id: app.version_id },
          },
        ]),
        headers: JSON_HEADERS,
      });
      return String(conversationId);
    },
    onSuccess: (conversationId) => {
      void queryClient.invalidateQueries({ queryKey: ['folder', 'list'] });
      void navigate({ to: '/chat/$conversationId', params: { conversationId } });
    },
  });

  return {
    start: projectId === undefined ? undefined : () => mutation.mutate(projectId),
    isStarting: mutation.isPending,
    isError: mutation.isError,
  };
}
