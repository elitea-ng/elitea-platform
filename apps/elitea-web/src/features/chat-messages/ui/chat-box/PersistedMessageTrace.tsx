import { useState, type ReactNode } from 'react';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Typography from '@mui/material/Typography';
import { useGetMessageTrace, useListMessageTraces } from '@/shared/api/generated/chat/chat';
import type { MessageTraceStep, MessageTraceStepDetail } from '@/shared/api/generated/model';
import { BasicAccordion } from '@/shared/ui/BasicAccordion';
import { t } from '@/shared/i18n';
import { CodeDebugTraceArtifact } from '@/shared/ui/CodeDebugArtifact';

interface TraceReference {
  readonly projectId: string;
  readonly conversationId: string;
  readonly messageGroupId: number;
  readonly steps: readonly MessageTraceStep[];
  readonly failed: boolean;
}

function readReference(value: unknown): TraceReference | undefined {
  if (typeof value !== 'object' || value === null) return undefined;
  const row = value as Partial<TraceReference>;
  if (typeof row.projectId !== 'string' || typeof row.conversationId !== 'string' || !Number.isSafeInteger(row.messageGroupId) || !Array.isArray(row.steps)) return undefined;
  return row as TraceReference;
}

function TraceDetail({ detail }: { readonly detail: MessageTraceStepDetail }): ReactNode {
  return <Box sx={{ whiteSpace: 'pre-wrap', overflowWrap: 'anywhere' }}>
    {detail.tool_inputs != null && <Typography component="pre">{JSON.stringify(detail.tool_inputs, null, 2)}</Typography>}
    {detail.tool_output != null && <Typography component="pre" sx={{ whiteSpace: 'pre-wrap' }}>{detail.tool_output}</Typography>}
    {detail.thinking && <Typography>{detail.thinking}</Typography>}
    {detail.text && <Typography>{detail.text}</Typography>}
  </Box>;
}

function traceStepName(step: MessageTraceStep): string {
  if (step.tool_name) return step.tool_name;
  const metadata = step.attrs?.['response_metadata'];
  if (typeof metadata === 'object' && metadata !== null && 'tool_name' in metadata
    && typeof metadata.tool_name === 'string' && metadata.tool_name.trim()) return metadata.tool_name;
  return step.model_name ?? step.kind;
}

function StepDetail({ reference, step }: { readonly reference: TraceReference; readonly step: MessageTraceStep }): ReactNode {
  const detail = useGetMessageTrace(reference.projectId, step.id, { message_group_id: reference.messageGroupId });
  const data = detail.data?.data as MessageTraceStepDetail | undefined;
  return <Box>
    {detail.isFetching && <Typography component="output">{t('common.loading', 'Loading...')}</Typography>}
    {detail.isError && <Box role="alert">{t('features.chatMessages.traceReadFailed', 'Execution details could not be loaded.')}<Button onClick={() => { void detail.refetch(); }}>{t('common.retry', 'Retry')}</Button></Box>}
    {data && <TraceDetail detail={data} />}
    {data?.id === step.id && data.message_group_id === reference.messageGroupId && <CodeDebugTraceArtifact
      attrs={data.attrs}
      projectId={reference.projectId}
      scopeKey={JSON.stringify([reference.conversationId, reference.messageGroupId, data.id])}
    />}
  </Box>;
}

function stepTitle(step: MessageTraceStep): string {
  return [step.parent_agent_name, traceStepName(step), step.is_error ? t('common.error', 'Error') : undefined].filter(Boolean).join(' · ');
}

/**
 * The steps of one answer. A single step is shown as it is, under its name —
 * a second fold inside "Execution details" would only hide the one thing there
 * is; several steps each fold, their detail fetched when opened.
 */
function TracePanel({ reference }: { readonly reference: TraceReference }): ReactNode {
  const [selected, setSelected] = useState<MessageTraceStep>();
  const summary = useListMessageTraces(reference.projectId, Number(reference.conversationId), { message_group_ids: String(reference.messageGroupId) }, { query: { enabled: false } });
  const listing = summary.data?.data as { rows?: MessageTraceStep[] } | undefined;
  const steps = listing?.rows ?? reference.steps;
  const only = steps.length === 1 ? steps[0] : undefined;
  return <Box>
    {reference.failed && !summary.isSuccess && <Box role="alert">
      {t('features.chatMessages.traceReadFailed', 'Execution details could not be loaded.')}
      <Button onClick={() => { void summary.refetch(); }} disabled={summary.isFetching}>{t('common.retry', 'Retry')}</Button>
    </Box>}
    {only !== undefined ? <Box data-testid="trace-single-step">
      <Typography variant="labelSmall" component="div" sx={{ color: 'text.secondary', mb: 0.5 }}>{stepTitle(only)}</Typography>
      <StepDetail reference={reference} step={only} />
    </Box> : steps.map(step => <BasicAccordion key={step.id} uppercase={false} expanded={selected?.id === step.id}
      onChange={(_event, expanded) => { setSelected(expanded ? step : undefined); }}
      items={[{
        title: stepTitle(step),
        content: selected?.id === step.id ? <StepDetail reference={reference} step={step} /> : null,
      }]} />)}
  </Box>;
}

export function PersistedMessageTrace({ value }: { readonly value: unknown }): ReactNode {
  const reference = readReference(value);
  if (!reference || (!reference.failed && reference.steps.length === 0)) return null;
  return <PersistedTraceFold reference={reference} />;
}

/** "Execution details", folded; its steps mount (and fetch) only once it is opened. */
function PersistedTraceFold({ reference }: { readonly reference: TraceReference }): ReactNode {
  const [open, setOpen] = useState(false);
  return <BasicAccordion uppercase={false} expanded={open} onChange={(_event, expanded) => { setOpen(expanded); }} items={[{
    title: t('features.chatMessages.executionDetails', 'Execution details'),
    content: open ? <TracePanel reference={reference} /> : null,
  }]} />;
}
