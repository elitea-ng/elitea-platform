/**
 * The trace half of the run-history panel (issue #868) — "open a trace" for
 * one conversation selected from `RunHistoryList`.
 *
 * Lists that conversation's `chat_message_trace_step` rows via the already-
 * landed `listMessageTraces` route (light projection: kind, tool name,
 * timing, error flag), and fetches one step's heavy fields (`tool_inputs`/
 * `tool_output`/`text`/`thinking`) via `getMessageTrace` on expand — the same
 * split the pin-strip-under-an-answer UI uses, so this reuses the generated
 * hooks rather than adding a second reader over the same table.
 */
import { useState, type ReactNode } from 'react';

import Box from '@mui/material/Box';
import Chip from '@mui/material/Chip';
import CircularProgress from '@mui/material/CircularProgress';
import List from '@mui/material/List';
import ListItemButton from '@mui/material/ListItemButton';
import ListItemText from '@mui/material/ListItemText';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { useGetMessageTrace, useListMessageTraces } from '@/shared/api/generated/chat/chat';
import type { MessageTraceStep, MessageTraceStepDetail } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { NoResultsMessage } from '@/shared/ui/NoResultsMessage';

export interface RunHistoryTraceProps {
  readonly projectId: string;
  readonly conversationId: string;
}

const containerSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.75rem', height: '100%' };
const listSx: SxProps<Theme> = { maxHeight: '100%', overflowY: 'auto' };
const detailBoxSx: SxProps<Theme> = {
  borderTop: 1,
  borderColor: 'divider',
  paddingTop: '0.75rem',
  maxHeight: '40%',
  overflowY: 'auto',
};

function stepLabel(step: MessageTraceStep): string {
  if (step.kind === 'tool_call' && step.tool_name !== null && step.tool_name !== undefined) return step.tool_name;
  if (step.parent_agent_name !== null && step.parent_agent_name !== undefined) return step.parent_agent_name;
  return step.kind;
}

/** The step list + selected step's detail — split out of `RunHistoryTrace` purely to keep that function's cyclomatic complexity under this codebase's gate (12). */
function StepList({
  steps,
  selectedStep,
  detail,
  onSelect,
}: {
  readonly steps: readonly MessageTraceStep[];
  readonly selectedStep: MessageTraceStep | undefined;
  readonly detail: MessageTraceStepDetail | undefined;
  readonly onSelect: (step: MessageTraceStep) => void;
}): ReactNode {
  return (
    <Box sx={containerSx} data-testid="run-history-trace">
      <List dense sx={listSx} data-testid="run-history-trace-list">
        {steps.map((step) => (
          <ListItemButton
            key={step.id}
            selected={step.id === selectedStep?.id}
            data-testid="run-history-trace-step"
            onClick={() => onSelect(step)}
          >
            <ListItemText
              primary={stepLabel(step)}
              secondary={step.started_at ?? undefined}
              slotProps={{ secondary: { variant: 'bodySmall' } }}
            />
            {step.is_error && (
              <Chip
                size="small"
                color="error"
                label={t('entities.runHistory.trace.errorChip', 'Error')}
              />
            )}
          </ListItemButton>
        ))}
      </List>
      {selectedStep !== undefined && detail !== undefined && <StepDetail detail={detail} />}
    </Box>
  );
}

function StepDetail({ detail }: { readonly detail: MessageTraceStepDetail }): ReactNode {
  return (
    <Box sx={detailBoxSx} data-testid="run-history-trace-detail">
      {detail.text !== null && detail.text !== undefined && detail.text !== '' && (
        <Typography variant="bodySmall" sx={{ whiteSpace: 'pre-wrap' }}>
          {detail.text}
        </Typography>
      )}
      {detail.tool_output !== null && detail.tool_output !== undefined && detail.tool_output !== '' && (
        <Typography variant="bodySmall" component="pre" sx={{ whiteSpace: 'pre-wrap', fontFamily: 'monospace' }}>
          {detail.tool_output}
        </Typography>
      )}
    </Box>
  );
}

export function RunHistoryTrace({ projectId, conversationId }: RunHistoryTraceProps): ReactNode {
  const [selectedStep, setSelectedStep] = useState<MessageTraceStep | undefined>(undefined);

  const tracesQuery = useListMessageTraces(
    projectId,
    Number(conversationId),
    { include_total: true },
    { query: { enabled: projectId !== '' && conversationId !== '' } },
  );
  // `.data.data`'s declared type includes the error-envelope variant — never
  // actually reachable here, since `eliteaFetch` throws instead of resolving
  // with it (mutator.ts's §3.6 unwrap contract) — same cast PrivateAgentsList
  // already establishes for a generated list hook.
  const listing = tracesQuery.data?.data as { readonly rows: readonly MessageTraceStep[] } | undefined;
  const steps = listing?.rows ?? [];

  const detailQuery = useGetMessageTrace(
    projectId,
    selectedStep?.id ?? 0,
    { message_group_id: selectedStep?.message_group_id ?? 0 },
    { query: { enabled: selectedStep !== undefined } },
  );
  const detail = detailQuery.data?.data as MessageTraceStepDetail | undefined;

  if (tracesQuery.isPending) {
    return (
      <Box sx={{ display: 'flex', justifyContent: 'center', padding: '1rem' }} data-testid="run-history-trace-loading">
        <CircularProgress size={24} />
      </Box>
    );
  }

  if (steps.length === 0) {
    return (
      <NoResultsMessage
        title={t('entities.runHistory.trace.emptyTitle', 'No trace steps')}
        description={t(
          'entities.runHistory.trace.empty',
          'This conversation has no recorded tool calls or thinking steps.',
        )}
      />
    );
  }

  return <StepList steps={steps} selectedStep={selectedStep} detail={detail} onSelect={setSelectedStep} />;
}
