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
import { toolPayloadText } from '@/shared/lib/toolPayloadText';
import { NoResultsMessage } from '@/shared/ui/NoResultsMessage';

import { formatRunDuration } from '../lib/formatRunDuration';

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

/** `attrs` is a bounded jsonb sidecar (see `messagetraces/handler.go`); narrow it to a plain object before indexing. */
function attrsRecord(value: unknown): Record<string, unknown> | undefined {
  return typeof value === 'object' && value !== null ? (value as Record<string, unknown>) : undefined;
}

/**
 * The toolkit name behind a tool-call step, read off `attrs` — nested under
 * either `metadata.toolkit_name` or `tool_meta.metadata.toolkit_name`,
 * because `agent_trace.go`'s `currentAgentToolCallAttrs` folds the worker's
 * `metadata` and its `tool_meta.metadata` separately and only whichever one
 * the emitting frame carried is populated (same two places
 * `currentAgentToolkitAttr` reads server-side).
 */
function toolkitNameFromAttrs(attrs: unknown): string | undefined {
  const record = attrsRecord(attrs);
  if (record === undefined) return undefined;
  const fromMetadata = attrsRecord(record['metadata'])?.['toolkit_name'];
  if (typeof fromMetadata === 'string' && fromMetadata !== '') return fromMetadata;
  const fromToolMeta = attrsRecord(attrsRecord(record['tool_meta'])?.['metadata'])?.['toolkit_name'];
  return typeof fromToolMeta === 'string' && fromToolMeta !== '' ? fromToolMeta : undefined;
}

/**
 * #938 (ELITEA-2803) — "[Toolkit Name]: [tool_action]", not the bare tool
 * name a `get_issue` call carries on its own. Falls back to the bare name
 * when no toolkit identity is on the row (an MCP tool with no toolkit
 * metadata, say) or when the toolkit name IS the tool name already.
 */
function stepLabel(step: MessageTraceStep): string {
  if (step.kind === 'tool_call' && step.tool_name !== null && step.tool_name !== undefined) {
    const toolkitName = toolkitNameFromAttrs(step.attrs);
    if (toolkitName !== undefined && toolkitName !== step.tool_name) return `${toolkitName}: ${step.tool_name}`;
    return step.tool_name;
  }
  if (step.parent_agent_name !== null && step.parent_agent_name !== undefined) return step.parent_agent_name;
  return step.kind;
}

/** The `attrs.tool_output_chunks` progress a chunked tool_output row carries (#956/#938). */
interface ChunkProgress {
  readonly received: number;
  readonly total: number;
  readonly complete: boolean;
}

function chunkProgress(attrs: unknown): ChunkProgress | undefined {
  const progress = attrsRecord(attrsRecord(attrs)?.['tool_output_chunks']);
  if (progress === undefined) return undefined;
  const received = progress['received'];
  const total = progress['total'];
  const complete = progress['complete'];
  if (typeof received !== 'number' || typeof total !== 'number' || typeof complete !== 'boolean') return undefined;
  return { received, total, complete };
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

/** JSON-stringifies `tool_inputs` for display; a non-object value (already a string, say) is shown as-is. */
function formatToolInputs(toolInputs: unknown): string {
  if (typeof toolInputs === 'string') return toolInputs;
  try {
    return JSON.stringify(toolInputs, null, 2);
  } catch {
    return String(toolInputs);
  }
}

/** Non-null-or-blank guard shared by every optional string field this pane renders. */
function definedString(value: string | null | undefined): string | undefined {
  return value !== null && value !== undefined && value !== '' ? value : undefined;
}

/** The step's start/finish timing — absent entirely when the row carries neither timestamp. */
function StepTiming({
  startedAt,
  finishedAt,
}: {
  readonly startedAt: string | null | undefined;
  readonly finishedAt: string | null | undefined;
}): ReactNode {
  const hasTiming = definedString(startedAt) !== undefined || definedString(finishedAt) !== undefined;
  if (!hasTiming) return null;
  return (
    <Typography variant="bodySmall" color="text.secondary" data-testid="run-history-trace-timing">
      {t('entities.runHistory.trace.timing', 'Started {{startedAt}} · {{duration}}', {
        startedAt: startedAt ?? '—',
        duration: formatRunDuration(startedAt ?? undefined, finishedAt ?? undefined),
      })}
    </Typography>
  );
}

function StepThinking({ thinking }: { readonly thinking: string | null | undefined }): ReactNode {
  const text = definedString(thinking);
  if (text === undefined) return null;
  return (
    <Typography
      variant="bodySmall"
      data-testid="run-history-trace-thinking"
      sx={{ whiteSpace: 'pre-wrap', fontStyle: 'italic' }}
    >
      {text}
    </Typography>
  );
}

function StepToolInputs({ toolInputs }: { readonly toolInputs: unknown }): ReactNode {
  if (toolInputs === null || toolInputs === undefined) return null;
  return (
    <Typography
      variant="bodySmall"
      component="pre"
      data-testid="run-history-trace-tool-inputs"
      sx={{ whiteSpace: 'pre-wrap', fontFamily: 'monospace' }}
    >
      {formatToolInputs(toolInputs)}
    </Typography>
  );
}

function StepText({ text }: { readonly text: string | null | undefined }): ReactNode {
  const value = definedString(text);
  if (value === undefined) return null;
  return (
    <Typography variant="bodySmall" sx={{ whiteSpace: 'pre-wrap' }}>
      {value}
    </Typography>
  );
}

/**
 * The tool call's output — chunk/partial-aware (#938/#956): a row still
 * filling in from `attrs.tool_output_chunks` must never render as if it were
 * the whole result, so the count is shown alongside it. Formatted (pretty
 * JSON) rather than the previous raw single-line dump (ELITEA-2805).
 */
function StepToolOutput({
  toolOutput,
  attrs,
}: {
  readonly toolOutput: string | null | undefined;
  readonly attrs: unknown;
}): ReactNode {
  const value = definedString(toolOutput);
  if (value === undefined) return null;
  const progress = chunkProgress(attrs);
  const isPartial = progress !== undefined && !progress.complete;
  return (
    <>
      {isPartial && (
        <Typography
          variant="bodySmall"
          data-testid="run-history-trace-output-partial"
          sx={{ color: 'warning.main', fontWeight: 600 }}
        >
          {t(
            'entities.runHistory.trace.partialOutput',
            'Partial output — some of this result did not arrive ({{received}}/{{total}} chunks).',
            { received: progress?.received ?? 0, total: progress?.total ?? 0 },
          )}
        </Typography>
      )}
      <Typography
        variant="bodySmall"
        component="pre"
        data-testid="run-history-trace-tool-output"
        sx={{ whiteSpace: 'pre-wrap', fontFamily: 'monospace' }}
      >
        {toolPayloadText(value)}
      </Typography>
    </>
  );
}

/**
 * #938 (ELITEA-2802/2803/2805) — `tool_inputs`/`thinking` are fetched by the
 * same `useGetMessageTrace` call `text`/`tool_output` already use; they were
 * simply never read here. Rendered ahead of the OUTPUT, the order a "called
 * with these parameters, thought this, produced this" trace reads naturally
 * in. Split into one small component per field (rather than inline `&&`
 * chains) to keep this function's own cyclomatic complexity under the gate.
 */
function StepDetail({ detail }: { readonly detail: MessageTraceStepDetail }): ReactNode {
  return (
    <Box sx={detailBoxSx} data-testid="run-history-trace-detail">
      <StepTiming startedAt={detail.started_at} finishedAt={detail.finished_at} />
      <StepThinking thinking={detail.thinking} />
      <StepToolInputs toolInputs={detail.tool_inputs} />
      <StepText text={detail.text} />
      <StepToolOutput toolOutput={detail.tool_output} attrs={detail.attrs} />
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
