/**
 * The heavy half of a restored trace pin (#951).
 *
 * `useConversationTraceSteps` lists a conversation's steps in the LIGHT
 * projection — labels, ordering, error flag — because `tool_inputs`,
 * `tool_output`, `text` and `thinking` are TOASTed columns that
 * `messagetraces/handler.go` deliberately serves one row at a time. That is
 * the right split for a transcript, and it leaves one gap: a pin the reader
 * OPENS has a body to show, and the listing does not carry it.
 *
 * So the step's row identity travels on the action
 * (`entities/message/lib/toolActions.ts`'s `traceStepIdentity`) and `ToolModal`
 * asks for that one row when it is opened. The project id comes through this
 * context rather than through four layers of props — `ChatMessageList` already
 * has it, and `ActionView`/`ToolModal` are otherwise entirely presentational.
 *
 * ── WHY A LOADER IN THE CONTEXT AND NOT A QUERY HOOK ───────────────────────
 *
 * `ToolModal` is a leaf presentational component with its own unit tests that
 * render it bare, under a theme and nothing else. A `useQuery` inside it would
 * make every one of those a QueryClient test and would make the component
 * untestable without one. The context carries the FETCH instead: with no
 * provider above it — a bare render, a Storybook story — the value is
 * `undefined`, nothing is requested, and the modal shows what the action
 * itself carries.
 *
 * A LIVE step never reaches this path either: it carries its own body and no
 * row identity, so the caller's `active` stays false.
 */
import type { ReactNode } from 'react';
import { createContext, useContext, useEffect, useMemo, useState } from 'react';

import { getMessageTrace } from '@/shared/api/generated/chat/chat';

/** @public The body of one persisted trace step. */
export interface TraceStepDetail {
  readonly toolInputs: unknown;
  readonly output: string;
}

/** @public Fetches one persisted step's heavy columns. */
export type TraceStepLoader = (stepId: number, messageGroupId: number) => Promise<TraceStepDetail>;

/**
 * @public Exported so a test can stand a fake loader over the modal without a
 * network layer — the production path always goes through
 * {@link TraceStepDetailProvider}.
 */
export const TraceStepDetailContext = createContext<TraceStepLoader | undefined>(undefined);

/** @public Props for {@link TraceStepDetailProvider}. */
export interface TraceStepDetailProviderProps {
  readonly projectId?: string | undefined;
  readonly children: ReactNode;
}

/** Lets every pin rendered underneath fetch its own step's body. */
export function TraceStepDetailProvider({ projectId, children }: TraceStepDetailProviderProps): ReactNode {
  const load = useMemo<TraceStepLoader | undefined>(() => {
    if (projectId === undefined || projectId === '') return undefined;
    return async (stepId, messageGroupId) => {
      const response = await getMessageTrace(projectId, stepId, { message_group_id: messageGroupId });
      // `.data`'s declared type includes the error-envelope variant, which
      // `eliteaFetch` throws rather than resolving with — the same cast
      // `RunHistoryTrace` establishes for this very endpoint.
      const detail = response.data as {
        readonly tool_inputs?: unknown;
        readonly tool_output?: string | null;
        readonly text?: string | null;
        readonly thinking?: string | null;
      };
      // `tool_output` for a tool call, `text` for a thinking step — a row is
      // one or the other, and `thinking` is the reasoning behind a step with
      // no text of its own.
      return { toolInputs: detail.tool_inputs, output: detail.tool_output || detail.text || detail.thinking || '' };
    };
  }, [projectId]);

  return <TraceStepDetailContext value={load}>{children}</TraceStepDetailContext>;
}

/**
 * One trace step's heavy columns, fetched only while `active` — which the
 * caller sets to "the pin is open AND it is a restored one AND it has no body
 * of its own". A failed read leaves the pin as it was rather than surfacing an
 * error over the transcript: the row itself is still true.
 */
export function useTraceStepDetail(
  step: { readonly traceStepId?: number; readonly traceMessageGroupId?: number } | undefined,
  active: boolean,
): TraceStepDetail | undefined {
  const load = useContext(TraceStepDetailContext);
  const [detail, setDetail] = useState<{ readonly stepId: number; readonly value: TraceStepDetail } | undefined>(
    undefined,
  );
  const stepId = step?.traceStepId;
  const messageGroupId = step?.traceMessageGroupId;

  useEffect(() => {
    if (!active || load === undefined || stepId === undefined || messageGroupId === undefined) return undefined;
    let live = true;
    void load(stepId, messageGroupId)
      .then((value) => {
        if (live) setDetail({ stepId, value });
      })
      .catch(() => {
        // Nothing to surface: the pin keeps saying what it already said.
      });
    return () => {
      live = false;
    };
  }, [active, load, stepId, messageGroupId]);

  // Guarded by the step it was fetched for, so a modal reused for another row
  // can never show the previous row's body.
  return detail !== undefined && detail.stepId === stepId ? detail.value : undefined;
}
