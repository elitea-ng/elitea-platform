import { useState, type ReactNode } from 'react';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Typography from '@mui/material/Typography';
import { useGetMessageTrace, useListMessageTraces } from '@/shared/api/generated/chat/chat';
import type { MessageTraceStep, MessageTraceStepDetail } from '@/shared/api/generated/model';
import { BasicAccordion } from '@/shared/ui/BasicAccordion';
import { t } from '@/shared/i18n';

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

function TracePanel({ reference }: { readonly reference: TraceReference }): ReactNode {
  const [selected, setSelected] = useState<MessageTraceStep>();
  const summary = useListMessageTraces(reference.projectId, Number(reference.conversationId), { message_group_ids: String(reference.messageGroupId) }, { query: { enabled: false } });
  const listing = summary.data?.data as { rows?: MessageTraceStep[] } | undefined;
  const steps = listing?.rows ?? reference.steps;
  const detail = useGetMessageTrace(reference.projectId, selected?.id ?? 0, { message_group_id: reference.messageGroupId }, { query: { enabled: selected !== undefined } });
  const data = detail.data?.data as MessageTraceStepDetail | undefined;
  return <Box>
    {reference.failed && !summary.isSuccess && <Box role="alert">
      {t('features.chatMessages.traceReadFailed', 'Execution details could not be loaded.')}
      <Button onClick={() => { void summary.refetch(); }} disabled={summary.isFetching}>{t('common.retry', 'Retry')}</Button>
    </Box>}
    {steps.map(step => <BasicAccordion key={step.id} uppercase={false} expanded={selected?.id === step.id}
      onChange={(_event, expanded) => { setSelected(expanded ? step : undefined); }}
      items={[{
        title: [step.parent_agent_name, step.tool_name ?? step.model_name ?? step.kind, step.is_error ? t('common.error', 'Error') : undefined].filter(Boolean).join(' · '),
        content: selected?.id === step.id ? <Box>
          {detail.isFetching && <Typography component="output">{t('common.loading', 'Loading...')}</Typography>}
          {detail.isError && <Box role="alert">{t('features.chatMessages.traceReadFailed', 'Execution details could not be loaded.')}<Button onClick={() => { void detail.refetch(); }}>{t('common.retry', 'Retry')}</Button></Box>}
          {data && <TraceDetail detail={data} />}
        </Box> : null,
      }]} />)}
  </Box>;
}

export function PersistedMessageTrace({ value }: { readonly value: unknown }): ReactNode {
  const reference = readReference(value);
  if (!reference || (!reference.failed && reference.steps.length === 0)) return null;
  return <BasicAccordion uppercase={false} defaultExpanded={false} items={[{
    title: t('features.chatMessages.executionDetails', 'Execution details'),
    content: <TracePanel reference={reference} />,
  }]} />;
}
