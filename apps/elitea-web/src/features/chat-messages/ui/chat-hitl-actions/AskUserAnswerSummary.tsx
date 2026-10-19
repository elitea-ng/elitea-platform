/**
 * Port of `apps/elitea-ui/src/[fsd]/features/chat/ui/chat-hitl-actions/
 * AskUserAnswerSummary.jsx` — the questions and answers of a completed
 * `ask_user` call, shown above the answer once the pause is resolved.
 *
 * A reloaded trace step carries no body of its own; like the baseline, it is
 * fetched once (through `useTraceStepDetail`, the same loader `ToolModal`
 * uses) only when the action has no inline data and a trace-step identity.
 * Without a `TraceStepDetailProvider` above it nothing is fetched.
 */
import type { ReactNode } from 'react';
import { useMemo } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { buildAskUserSummary, findAskUserAction } from '../../lib/askUserSummary';
import { useTraceStepDetail } from '../../model/traceStepDetail';

/** The tool-action fields the summary reads (a `ToolActionDraft` / `SubAgentGroupable`). */
interface AskUserToolAction {
  readonly name?: unknown;
  readonly status?: unknown;
  readonly toolInputs?: unknown;
  readonly toolOutputs?: unknown;
  readonly content?: unknown;
  readonly traceStepId?: number;
  readonly traceMessageGroupId?: number;
}

/** @public Props for `AskUserAnswerSummary`. */
export interface AskUserAnswerSummaryProps {
  readonly toolActions?: readonly AskUserToolAction[] | null | undefined;
}

/** `AskUserAnswerSummary` — numbered questions with the answer under each. */
export function AskUserAnswerSummary({ toolActions }: AskUserAnswerSummaryProps): ReactNode {
  const action = useMemo(() => findAskUserAction(toolActions), [toolActions]);
  const hasInlineData = Boolean(action?.toolInputs || action?.toolOutputs || action?.content);
  const needsFetch = Boolean(action && !hasInlineData && action.traceStepId !== undefined);
  const fetched = useTraceStepDetail(action ?? undefined, needsFetch);

  const items = useMemo(() => {
    if (!action) return null;
    const toolInputs = fetched?.toolInputs ?? action.toolInputs;
    const toolOutputs = fetched?.output ?? action.toolOutputs ?? action.content;
    return buildAskUserSummary(toolInputs, toolOutputs);
  }, [action, fetched]);

  if (!items?.length) return null;

  return (
    <Box data-testid="ask-user-answer-summary" sx={containerSx}>
      {items.map((item, index) => (
        <Box key={`${index}-${item.question}`} sx={itemSx}>
          <Typography variant="bodyMedium" sx={questionSx}>
            {`${index + 1}. ${item.question}`}
          </Typography>
          {item.answer && (
            <Typography variant="bodySmall" sx={answerSx}>
              {item.answer}
            </Typography>
          )}
        </Box>
      ))}
    </Box>
  );
}

const containerSx: SxProps<Theme> = (theme) => ({
  width: '100%',
  display: 'flex',
  flexDirection: 'column',
  gap: '1rem',
  padding: '1rem 1.25rem',
  marginBottom: '1rem',
  borderRadius: theme.vars.shape.radiusMd,
  background: theme.vars.palette.background.secondary,
  border: `0.0625rem solid ${theme.vars.palette.border.cardsOutlines}`,
});
const itemSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.125rem' };
const questionSx: SxProps<Theme> = (theme) => ({ fontWeight: 600, color: theme.vars.palette.text.primary });
const answerSx: SxProps<Theme> = (theme) => ({ color: theme.vars.palette.text.secondary });
