import { useCallback, useRef, useState } from 'react';

import type { DraftTurn } from '@elitea/client-core/agent-builder';

import { hasBackendCapability } from '@/shared/config/backendCapabilities';
import { t } from '@/shared/i18n';

import { useGenerateAgentDraftMutation } from '../../api/generateAgentDraft';
import { useAgentDraftStream } from '../../api/generateAgentDraftStream';
import { applicationErrorMessage } from '../../lib/errorMessage';
import { mapApplicationDraft, type AgentDraft } from '../../lib/agentDraft';

/**
 * One "generate" for the Build with AI modal, streamed when this build serves
 * the stream (`aiDraftStreaming`, ADR-0037) and one-shot otherwise.
 *
 * The streamed builder is a conversation: every request and the draft it
 * produced are kept as turns, and a refinement sends them with the current
 * draft, so "make it also name the on-call engineer" revises the draft
 * instead of starting over. The one-shot route has no history, so
 * `canRefine` is false there.
 */
export interface AgentDraftGenerator {
  readonly generate: (request: string) => Promise<AgentDraft | undefined>;
  readonly refine: (request: string, current: AgentDraft) => Promise<AgentDraft | undefined>;
  readonly canRefine: boolean;
  /** The model's text so far while a streamed draft arrives; empty otherwise. */
  readonly streamText: string;
  readonly isGenerating: boolean;
  readonly error: string | undefined;
  /** Stops a generation in flight and forgets the conversation. */
  readonly reset: () => void;
}

const summary = (draft: AgentDraft): string => `${draft.name}: ${draft.description}`;

/** Keeps a request and the draft it produced as two turns of the builder conversation. */
function remember(turns: { current: DraftTurn[] }, request: string, draft: AgentDraft | undefined): AgentDraft | undefined {
  if (draft !== undefined) turns.current = [...turns.current, { role: 'user', content: request }, { role: 'assistant', content: summary(draft) }];
  return draft;
}

export function useAgentDraftGenerator(projectId: string | undefined): AgentDraftGenerator {
  const streaming = hasBackendCapability('aiDraftStreaming');
  const oneShot = useGenerateAgentDraftMutation();
  const stream = useAgentDraftStream(projectId);
  const turns = useRef<DraftTurn[]>([]);
  const [oneShotBusy, setOneShotBusy] = useState(false);

  const generate = useCallback(
    async (request: string): Promise<AgentDraft | undefined> => {
      turns.current = [];
      if (streaming) return remember(turns, request, await stream.generate({ request }));
      if (projectId === undefined) return undefined;
      setOneShotBusy(true);
      try {
        const response = await oneShot.generateDraft({ projectId, user_description: request });
        return response === undefined ? undefined : mapApplicationDraft(response);
      } finally {
        setOneShotBusy(false);
      }
    },
    [streaming, stream, oneShot, projectId],
  );

  const refine = useCallback(
    async (request: string, current: AgentDraft): Promise<AgentDraft | undefined> => {
      if (!streaming) return undefined;
      const draft = await stream.generate({
        request,
        history: turns.current,
        current: {
          name: current.name,
          description: current.description,
          instructions: current.instructions,
          welcome_message: current.welcome_message,
          conversation_starters: current.conversation_starters,
        },
      });
      return remember(turns, request, draft);
    },
    [streaming, stream],
  );

  const failure = stream.state.phase === 'failed' ? stream.state.failure.message : undefined;
  const oneShotError = oneShot.error === undefined ? undefined : applicationErrorMessage(oneShot.error) || t('features.agents.generateAgentModal.generateFailed', 'Failed to generate. Please try again.');

  return {
    generate,
    refine,
    canRefine: streaming,
    streamText: stream.state.phase === 'streaming' ? stream.state.text : '',
    isGenerating: streaming ? stream.state.phase === 'streaming' : oneShotBusy,
    error: streaming ? failure : oneShotError,
    reset: () => {
      turns.current = [];
      stream.reset();
      oneShot.reset();
    },
  };
}
