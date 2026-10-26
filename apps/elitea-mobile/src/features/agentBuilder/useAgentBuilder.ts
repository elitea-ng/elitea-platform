import { useQueryClient } from '@tanstack/react-query';
import { useCallback, useEffect, useReducer, useRef, useState } from 'react';

import { sessionApiClient, type ApiClient } from '@/api/client';
import { unwrap } from '@/api/errors';
import type { components } from '@/api/generated/elitea.gen';
import { startConversationWithAgent } from '@/api/queries/agents';
import { queryKeys } from '@/api/queries/keys';
import {
  INITIAL_DRAFT_STATE,
  agentCreateBody,
  draftReducer,
  recordTurn,
  type AgentCreateInput,
  type DraftAction,
  type DraftState,
  type DraftTurn,
} from '@elitea/client-core/agent-builder';

import { streamAgentDraft } from './streamDraft';

/**
 * The mobile agent builder (ADR-0037, the Elitea branch only: a phone has no
 * folder for an AGENTS.md). A request streams a draft; Refine sends the next
 * turn with the conversation and the draft as edited; Save creates the agent
 * through the reviewed create route, refreshes the agents list, and starts a
 * chat with it, so the new agent is usable at once.
 */

/** The fields a person reviews and edits before saving. */
export type EditableDraft = AgentCreateInput;

type CreateApplicationRequest = components['schemas']['ApplicationCreateRequest'];

/** Creates the agent; returns its id. */
export async function createAgent(projectId: number, draft: EditableDraft, client: ApiClient = sessionApiClient()): Promise<number> {
  const created = await unwrap(
    client.POST('/elitea_core/applications/prompt_lib/{project_id}', {
      params: { path: { project_id: String(projectId) } },
      body: agentCreateBody(draft) as CreateApplicationRequest,
    }),
  );
  const id = Number(created?.id);
  if (!Number.isSafeInteger(id) || id <= 0) throw new Error('the create route answered without an id');
  return id;
}

export interface AgentBuilder {
  state: DraftState;
  generate: (request: string) => void;
  refine: (request: string, current: EditableDraft) => void;
  /** Saves the draft and starts a chat with it; resolves to the new conversation's id, or null on failure (see `saveError`). */
  save: (draft: EditableDraft) => Promise<number | null>;
  saving: boolean;
  saveError: string | null;
  reset: () => void;
}

/** The edit-sensitive fields of a draft: what a save sent. */
const fingerprint = (draft: EditableDraft): string => JSON.stringify(agentCreateBody(draft));

export function useAgentBuilder(projectId: number | null): AgentBuilder {
  const queryClient = useQueryClient();
  const [state, dispatch] = useReducer(draftReducer, INITIAL_DRAFT_STATE);
  const controller = useRef<AbortController | null>(null);
  const turns = useRef<DraftTurn[]>([]);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);
  // The agent a save already created: a retry after a failed chat start must not create it twice.
  const savedId = useRef<number | null>(null);
  // What that save sent, so a retry with an edited draft is not answered with the stale agent.
  const savedFingerprint = useRef<string | null>(null);

  const cancel = useCallback(() => {
    controller.current?.abort();
    controller.current = null;
  }, []);
  useEffect(() => cancel, [cancel]);

  const run = useCallback(
    (request: string, current?: EditableDraft) => {
      const trimmed = request.trim();
      if (projectId === null || trimmed === '') return;
      cancel();
      const own = new AbortController();
      controller.current = own;
      const history = current === undefined ? [] : turns.current;
      if (current === undefined) turns.current = [];
      const fold = (action: DraftAction) => {
        if (own.signal.aborted) return;
        if (action.type === 'complete') {
          turns.current = recordTurn(history, trimmed, action.draft);
        }
        dispatch(action);
      };
      dispatch({ type: 'start' });
      savedId.current = null;
      savedFingerprint.current = null;
      setSaveError(null);
      void streamAgentDraft({ projectId, input: { request: trimmed, history, current }, signal: own.signal, dispatch: fold }).finally(() => {
        if (controller.current === own) controller.current = null;
      });
    },
    [projectId, cancel],
  );

  const save = useCallback(
    async (draft: EditableDraft): Promise<number | null> => {
      if (projectId === null || saving) return null;
      setSaving(true);
      setSaveError(null);
      try {
        // The chat start failed after the agent was saved, and the person has
        // edited the draft since: starting that chat would silently drop the
        // edits, so the edited draft is saved as the agent instead.
        if (savedId.current !== null && savedFingerprint.current !== fingerprint(draft)) savedId.current = null;
        if (savedId.current === null) {
          savedId.current = await createAgent(projectId, draft);
          savedFingerprint.current = fingerprint(draft);
          void queryClient.invalidateQueries({ queryKey: queryKeys.agents(projectId) });
        }
        const id = savedId.current;
        const conversation = await startConversationWithAgent(projectId, { id, name: draft.name.trim(), description: draft.description.trim() });
        void queryClient.invalidateQueries({ queryKey: queryKeys.conversations(projectId) });
        return conversation.id;
      } catch {
        setSaveError(
          savedId.current === null
            ? "Couldn't save the agent. Check your connection and try again."
            : "The agent is saved, but a chat with it couldn't start. Try again, or pick it in New chat.",
        );
        return null;
      } finally {
        setSaving(false);
      }
    },
    [projectId, saving, queryClient],
  );

  return {
    state,
    generate: (request) => run(request),
    refine: (request, current) => run(request, current),
    save,
    saving,
    saveError,
    reset: () => {
      cancel();
      turns.current = [];
      savedId.current = null;
      savedFingerprint.current = null;
      dispatch({ type: 'reset' });
    },
  };
}
