/**
 * "Mira remembered: …" under an answer whose agent saved a note to its memory
 * (ADR-0050 decision 1b, issue 1608), with Edit and Forget, and Approve for a
 * note that waits for approval when the reader may approve it.
 *
 * WHERE THE NOTE COMES FROM. The answer's own `remember` tool calls: each
 * completed call whose output names an `entry_id` is one chip. A reloaded
 * trace step carries no body; like `AskUserAnswerSummary`, the body is fetched
 * once through `useTraceStepDetail` when it has a trace-step identity (and
 * nothing is fetched without a `TraceStepDetailProvider` above).
 *
 * WHAT THE READER MAY DO is the server's decision. Edit and Forget are offered
 * to everyone: the server lets the person a note is about correct or delete
 * it, and a manager any note; anyone else gets its refusal, shown on the chip.
 * Approve asks the server first (`can_approve`) and is shown only when yes.
 * Once forgotten, the chip says so and offers nothing more.
 */
import type { ReactNode } from 'react';
import { useMemo, useState } from 'react';

import BookmarkAddedOutlinedIcon from '@mui/icons-material/BookmarkAddedOutlined';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';

import { memoryErrorMessage } from '../lib/memoryFilters';
import { rememberActions, rememberedEntryOf, type RememberToolAction, type RememberedEntry } from '../lib/rememberedEntries';
import { useAgentMemoryAbilities, useAgentMemoryWrites } from '../model/useAgentMemory';
import { MemoryEditDialog } from './MemoryEditDialog';

/** Loads a reloaded step's body (`useTraceStepDetail`), injected by the chat so this slice does not import it. */
export type TraceStepBodyHook = (
  step: { readonly traceStepId?: number; readonly traceMessageGroupId?: number } | undefined,
  active: boolean,
) => { readonly toolInputs?: unknown; readonly output?: unknown } | undefined;

export interface RememberedChipsProps {
  readonly toolActions: readonly RememberToolAction[] | undefined;
  readonly projectId: string | undefined;
  readonly applicationId: number | undefined;
  /** The agent's name as the chat shows it (the persona's display name first). */
  readonly agentName: string | undefined;
  readonly useStepBody: TraceStepBodyHook;
}

const chipSx: SxProps<Theme> = (theme) => ({
  display: 'flex',
  flexWrap: 'wrap',
  alignItems: 'center',
  gap: '0.5rem',
  padding: '0.5rem 0.75rem',
  marginBottom: '0.75rem',
  borderRadius: theme.vars.shape.radiusMd,
  background: theme.vars.palette.background.secondary,
  border: `0.0625rem solid ${theme.vars.palette.border.cardsOutlines}`,
});

export function RememberedChips({ toolActions, projectId, applicationId, agentName, useStepBody }: RememberedChipsProps): ReactNode {
  const actions = useMemo(() => rememberActions(toolActions), [toolActions]);
  if (projectId === undefined || projectId === '' || applicationId === undefined || actions.length === 0) return null;
  return (
    <>
      {actions.map((action, index) => (
        <RememberedChip
          key={typeof action.id === 'string' ? action.id : index}
          action={action}
          projectId={projectId}
          applicationId={applicationId}
          agentName={agentName}
          useStepBody={useStepBody}
        />
      ))}
    </>
  );
}

interface RememberedChipProps {
  readonly action: RememberToolAction;
  readonly projectId: string;
  readonly applicationId: number;
  readonly agentName: string | undefined;
  readonly useStepBody: TraceStepBodyHook;
}

function RememberedChip({ action, projectId, applicationId, agentName, useStepBody }: RememberedChipProps): ReactNode {
  const needsFetch = !action.toolOutputs && !action.content && action.traceStepId !== undefined;
  const fetched = useStepBody(action, needsFetch);
  const entry = rememberedEntryOf(fetched?.toolInputs ?? action.toolInputs, fetched?.output ?? action.toolOutputs ?? action.content);
  if (entry === undefined) return null;
  return <RememberedEntryChip entry={entry} projectId={projectId} applicationId={applicationId} agentName={agentName} />;
}

interface RememberedEntryChipProps {
  readonly entry: RememberedEntry;
  readonly projectId: string;
  readonly applicationId: number;
  readonly agentName: string | undefined;
}

function RememberedEntryChip({ entry, projectId, applicationId, agentName }: RememberedEntryChipProps): ReactNode {
  const { update, remove } = useAgentMemoryWrites(projectId, applicationId);
  const { canApprove } = useAgentMemoryAbilities(projectId, applicationId, entry.status === 'pending');
  const [state, setState] = useState<{ readonly content: string; readonly status: RememberedEntry['status'] | 'forgotten' }>({
    content: entry.content,
    status: entry.status,
  });
  const [editing, setEditing] = useState(false);
  const [error, setError] = useState<string | undefined>(undefined);
  const name = agentName ?? t('features.agentMemory.chip.theAgent', 'The agent');
  const testId = `agent-memory-chip-${entry.entryId}`;

  if (state.status === 'forgotten') {
    return (
      <Box sx={chipSx} data-testid={testId} data-state="forgotten">
        <Typography variant="bodySmall" color="text.secondary">
          {t('features.agentMemory.chip.forgotten', '{{name}} forgot this.', { name })}
        </Typography>
      </Box>
    );
  }

  const forget = (): void => {
    setError(undefined);
    remove.mutate(entry.entryId, {
      onSuccess: () => setState((previous) => ({ ...previous, status: 'forgotten' })),
      onError: (cause) => setError(memoryErrorMessage(cause, t('features.agentMemory.chip.forgetFailed', 'It was not forgotten. Try again.'))),
    });
  };
  const approve = (): void => {
    setError(undefined);
    update.mutate(
      { entryId: entry.entryId, change: { status: 'active' } },
      {
        onSuccess: (saved) => setState({ content: saved.content, status: 'active' }),
        onError: (cause) => setError(memoryErrorMessage(cause, t('features.agentMemory.chip.approveFailed', 'It was not approved. Try again.'))),
      },
    );
  };

  return (
    <Box sx={chipSx} data-testid={testId} data-state={state.status}>
      <BookmarkAddedOutlinedIcon fontSize="small" color="action" />
      <Typography variant="bodySmall" sx={{ flex: '1 1 12rem', minWidth: 0, overflowWrap: 'anywhere' }} data-testid={`${testId}-text`}>
        {t('features.agentMemory.chip.remembered', '{{name}} remembered: {{content}}', { name, content: state.content })}
        {state.status === 'pending' && ` (${t('features.agentMemory.chip.pending', 'waiting for approval')})`}
      </Typography>
      {state.status === 'pending' && canApprove && (
        <Button size="small" onClick={approve} disabled={update.isPending} data-testid={`${testId}-approve`}>
          {t('features.agentMemory.action.approve', 'Approve')}
        </Button>
      )}
      <Button size="small" onClick={() => setEditing(true)} disabled={remove.isPending} data-testid={`${testId}-edit`}>
        {t('features.agentMemory.action.edit', 'Edit')}
      </Button>
      <Button size="small" color="error" onClick={forget} disabled={remove.isPending} data-testid={`${testId}-forget`}>
        {t('features.agentMemory.chip.forget', 'Forget')}
      </Button>
      {error !== undefined && (
        <Typography role="alert" variant="bodySmall" color="error" sx={{ flexBasis: '100%' }} data-testid={`${testId}-error`}>
          {error}
        </Typography>
      )}
      <MemoryEditDialog
        projectId={projectId}
        applicationId={applicationId}
        entry={editing ? { id: entry.entryId, content: state.content } : undefined}
        canChangeKind={false}
        onClose={() => setEditing(false)}
        onSaved={(saved) => setState({ content: saved.content, status: saved.status === 'pending' ? 'pending' : 'active' })}
      />
    </Box>
  );
}
