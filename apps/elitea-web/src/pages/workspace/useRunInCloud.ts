/**
 * "Run in cloud" for a pipeline (#1239 A3). A pipeline does not run on this
 * computer (the host refuses it with `pipeline_unsupported`), so the
 * workspace starts it as an ordinary cloud chat instead:
 *
 * 1. a NORMAL conversation (not Local work: any web Chats window continues
 *    it), private, named after the prompt;
 * 2. the USER and APPLICATION participants, as "Chat with pipeline" adds them;
 * 3. the turn, on the chat's own execution route
 *    (`agent.execute.application.v1`, the body the web chat sends);
 * 4. the chat page opens it, and re-attaches to the running turn
 *    (`chatStreamReattach`, #6654).
 *
 * Nothing runs on the desktop host. A failure before step 3 leaves at most an
 * empty conversation; the error is shown above the composer.
 */
import { useCallback, useRef, useState } from 'react';

import { useNavigate } from '@tanstack/react-router';

import { conversationApi } from '@/entities/conversation';
import { useAddParticipantMutation } from '@/entities/participant';
import { getExecutionTokens } from '@/features/mcps';
import type { WorkspaceTurn } from '@/features/workspace';
import { t } from '@/shared/i18n';

import { isCloudOnlyPick, type AgentSelection } from './useAgentSelection';
import { agentParticipants, useSignedInUserId } from './useEnsureConversation';
import type { WorkspaceComposerProps } from './WorkspaceComposer';

const MAX_NAME = 80;

export interface RunInCloud {
  /** Starts `prompt` on the picked pipeline in the cloud and opens its chat; `false` when it did not start (see `error`). */
  run: (prompt: string) => Promise<boolean>;
  running: boolean;
  error: string | null;
  /**
   * What the cloud run needs, and nothing of the local session (nothing runs on this computer): a picked agent and
   * version, a signed-in user, a project, and no run in flight.
   */
  canRun: boolean;
}

export function useRunInCloud(projectId: number, selection: AgentSelection): RunInCloud {
  const navigate = useNavigate();
  const { mutateAsync: createConversation } = conversationApi.useCreate();
  const { mutateAsync: addParticipants } = useAddParticipantMutation();
  const userId = useSignedInUserId();
  const [running, setRunning] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // Synchronous: a second click lands before React re-renders with `running`.
  const inFlight = useRef(false);
  const agent = selection.agents.find((a) => a.id === selection.agentId);
  const version = selection.versions.find((v) => String(v.id) === selection.versionId);

  const run = useCallback(
    async (prompt: string): Promise<boolean> => {
      if (agent === undefined || version === undefined || userId === undefined || prompt.trim() === '' || inFlight.current) return false;
      inFlight.current = true;
      setRunning(true);
      setError(null);
      try {
        const conversation = await createConversation({ projectId, name: prompt.trim().slice(0, MAX_NAME) || agent.name, is_private: true });
        const conversationId = String(conversation.id);
        const added = await addParticipants({
          projectId,
          conversationId,
          participants: agentParticipants(userId, { projectId, applicationId: Number(agent.id), applicationName: agent.name, versionId: version.id, agentType: 'pipeline' }),
        });
        const participant = added.find((p) => p.entityName !== 'user');
        const uuid = conversation.uuid ?? (await conversationApi.details({ projectId, id: conversationId })).uuid;
        if (participant === undefined || uuid === undefined) throw new Error('missing participant or conversation uuid');
        await conversationApi.startAgentExecution({
          projectId,
          conversationUuid: uuid,
          contract: conversationApi.contracts.application,
          body: {
            project_id: projectId,
            conversation_uuid: uuid,
            question_id: crypto.randomUUID(),
            interaction_uuid: crypto.randomUUID(),
            mcp_tokens: await getExecutionTokens(String(projectId)),
            participant_id: Number(participant.id),
            payload: { user_input: prompt },
          },
        });
        void navigate({ to: '/chat/$conversationId', params: { conversationId } });
        return true;
      } catch {
        setError(t('workspace.runInCloud.failed', 'The pipeline could not be started in the cloud. Try again, or run it from Chat.'));
        return false;
      } finally {
        inFlight.current = false;
        setRunning(false);
      }
    },
    [agent, version, userId, projectId, createConversation, addParticipants, navigate],
  );

  const canRun = agent !== undefined && version !== undefined && userId !== undefined && projectId > 0 && !running;
  return { run, running, error, canRun };
}

/** The composer when the pick is a pipeline: the send runs it in the cloud and opens its chat, gated on the cloud run alone. */
export function pipelineComposerBinding(run: RunInCloud): Pick<WorkspaceComposerProps, 'canSend' | 'busy' | 'onSend' | 'onStop' | 'placeholder' | 'showMode' | 'acceptImages'> {
  return {
    canSend: run.canRun,
    busy: run.running,
    onSend: (prompt) => run.run(prompt),
    onStop: () => undefined,
    placeholder: t('workspace.runInCloud.placeholder', 'Run this pipeline in the cloud…'),
    showMode: false,
    acceptImages: false,
  };
}

/** The composer's view of pipelines: whether the pick is one, the refused prompt to offer, and the turn error left to show. */
export interface ComposerPipeline {
  /** The pick is a pipeline: a send runs it in the cloud. */
  pipeline: boolean;
  run: RunInCloud;
  /** Remember a local send's prompt: "Run in cloud" sends it after a `pipeline_unsupported` refusal. */
  remember: (prompt: string) => void;
  /** The prompt the host refused as a pipeline; `null` when none was. */
  refusedPrompt: string | null;
  /** The turn's start error, unless it is that refusal (the offer stands in for it). */
  turnError: string | null;
}

export function useComposerPipeline(projectId: number, selection: AgentSelection, turn: Pick<WorkspaceTurn, 'startError' | 'startErrorCode'>, cloudMode: boolean): ComposerPipeline {
  const run = useRunInCloud(projectId, selection);
  const [lastPrompt, setLastPrompt] = useState<string | null>(null);
  const refusedPrompt = !cloudMode && turn.startErrorCode === 'pipeline_unsupported' ? lastPrompt : null;
  return {
    pipeline: !cloudMode && isCloudOnlyPick(selection),
    run,
    remember: setLastPrompt,
    refusedPrompt,
    turnError: refusedPrompt === null ? turn.startError : null,
  };
}
