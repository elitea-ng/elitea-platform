import { useCallback, useEffect, useMemo, useReducer, useRef } from 'react';

import {
  INITIAL_DRAFT_STATE,
  draftReducer,
  draftRequestBody,
  draftStreamReader,
  networkFailure,
  refusalFailure,
  type DraftAction,
  type DraftRequestInput,
  type DraftState,
  type DraftStreamFailure,
  type StreamedAgentDraft,
} from '@elitea/client-core/agent-builder';
import { EventStreamParser } from '@elitea/client-core/stream';

import { EliteaApiError, eliteaFetch } from '@/shared/api/generated/mutator';

/**
 * The conversational agent builder's draft, streamed (ADR-0037):
 * `generateApplicationDraftStream`,
 * `POST /elitea_core/generate_application_draft_stream/prompt_lib/{projectId}`.
 *
 * The request goes through `eliteaFetch` with the `stream` transport flag, so
 * it takes the app's one HTTP path (`shared/api/http.ts`): the session cookie
 * on the web, the bearer and its refresh on the desktop. The body arrives
 * unread and is parsed with the shared `EventStreamParser`; the states are
 * the shared `draftReducer` the mobile builder folds too.
 *
 * A failure before the stream opens (400, 403, 503) is an `EliteaApiError`;
 * its JSON body's `error`/`code` become the failure shown. After it opens, a
 * failure is the stream's own `draft.error`.
 */
export interface AgentDraftStream {
  readonly state: DraftState;
  /** Runs one generation; resolves to the draft, or undefined when it failed or was cancelled. */
  readonly generate: (input: DraftRequestInput) => Promise<StreamedAgentDraft | undefined>;
  readonly cancel: () => void;
  readonly reset: () => void;
}

/** The failure an HTTP refusal before the stream, or a thrown error, becomes. */
function failureOf(error: unknown): DraftStreamFailure {
  if (error instanceof EliteaApiError && error.failure.kind === 'http') return refusalFailure(error.failure.status, error.failure.body);
  return networkFailure(error);
}

export function useAgentDraftStream(projectId: string | undefined): AgentDraftStream {
  const [state, dispatch] = useReducer(draftReducer, INITIAL_DRAFT_STATE);
  const controller = useRef<AbortController | null>(null);

  const cancel = useCallback(() => {
    controller.current?.abort();
    controller.current = null;
  }, []);
  useEffect(() => cancel, [cancel]);

  const generate = useCallback(
    async (input: DraftRequestInput): Promise<StreamedAgentDraft | undefined> => {
      if (projectId === undefined || input.request.trim() === '') return undefined;
      cancel();
      const own = new AbortController();
      controller.current = own;
      let result: StreamedAgentDraft | undefined;
      const fold = (action: DraftAction): void => {
        if (own.signal.aborted) return;
        if (action.type === 'complete') result = action.draft;
        dispatch(action);
      };
      dispatch({ type: 'start' });
      try {
        const response = await eliteaFetch<{ data: ReadableStream<Uint8Array> | null }>(
          `/elitea_core/generate_application_draft_stream/prompt_lib/${projectId}`,
          { method: 'POST', headers: { 'Content-Type': 'application/json', Accept: 'text/event-stream' }, body: JSON.stringify(draftRequestBody(input)), signal: own.signal },
          { stream: true },
        );
        const reader = draftStreamReader(fold);
        const parser = new EventStreamParser();
        const body = response.data?.getReader();
        while (body !== undefined) {
          const chunk = await body.read();
          if (chunk.done) break;
          reader.feed(parser.push(chunk.value));
        }
        reader.feed(parser.end());
        reader.finish();
      } catch (error) {
        if (own.signal.aborted) return undefined;
        fold({ type: 'error', failure: failureOf(error) });
      } finally {
        if (controller.current === own) controller.current = null;
      }
      return own.signal.aborted ? undefined : result;
    },
    [projectId, cancel],
  );

  const reset = useCallback(() => {
    cancel();
    dispatch({ type: 'reset' });
  }, [cancel]);

  // Stable between renders unless the state moves, so a caller can depend on it.
  return useMemo(() => ({ state, generate, cancel, reset }), [state, generate, cancel, reset]);
}
