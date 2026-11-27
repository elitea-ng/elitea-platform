/**
 * The project instructions of a Local work thread in its Participants tab (issue 3130): the folder's AGENTS.md
 * (and SOUL.md, and the nested AGENTS.md the turn on screen applied) listed once, here, instead of a notice under
 * every message, with the thread's "Respect AGENTS.md" switch.
 *
 * The switch is the thread's own setting, kept by the desktop host: on by default, shown only when the folder holds
 * project instructions, and disabled until the host has said what the thread keeps. Before the first message the
 * choice is held for the thread the message creates (`pendingRespect`). A change applies from the thread's next
 * turn, for every agent of the thread; a turn already running keeps what it started with. When it is off the
 * section says so plainly. Each turn records whether it applied the files (the `running` status and the
 * commit's `local_work.project_instructions`).
 */
import Box from '@mui/material/Box';
import FormControlLabel from '@mui/material/FormControlLabel';
import Switch from '@mui/material/Switch';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';
import { keepPreviousData, useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useEffect, type ReactNode } from 'react';

import type { WorkspaceTurn } from '@/features/workspace';
import type { ThreadInstructionsCommands } from '@/shared/desktop/threadInstructionsIpc';
import { t } from '@/shared/i18n';

import { usePendingRespect } from './pendingRespect';

export interface ProjectInstructionsSectionProps {
  readonly workspaceId: string;
  /** The thread's conversation; '' before its first message. */
  readonly conversationId: string;
  /** The turn on screen: its `running` status names the nested AGENTS.md it applied too. */
  readonly turn: Pick<WorkspaceTurn, 'turnId' | 'view'>;
  readonly ipc: ThreadInstructionsCommands;
}

const KEY = ['desktop', 'thread-instructions'] as const;

/** The root's files first, then the ones only the turn on screen found (nested), each once. */
function listed(root: readonly string[], turn: readonly string[] | undefined): string[] {
  return [...new Set([...root, ...(turn ?? [])])];
}

interface SwitchInputs {
  status: { data: { respect: boolean } | undefined; isPlaceholderData: boolean };
  save: { isPending: boolean; variables: boolean | undefined };
}

/**
 * The switch's position: the pending choice before the first message (`pending`), else what the host keeps (or the
 * change being saved). Unknown until the host answers for this thread: shown on (the default) and disabled, never a
 * guess the person can flip.
 */
function switchState(pending: boolean | undefined, status: SwitchInputs['status'], save: SwitchInputs['save']): { respect: boolean; disabled: boolean } {
  if (pending !== undefined) return { respect: pending, disabled: false };
  const kept = save.isPending ? save.variables : status.data?.respect;
  return { respect: kept ?? true, disabled: status.data === undefined || status.isPlaceholderData || save.isPending };
}

function useInstructions({ workspaceId, conversationId, turn, ipc }: ProjectInstructionsSectionProps) {
  const queryClient = useQueryClient();
  const thread = conversationId === '' ? undefined : conversationId;
  const status = useQuery({
    queryKey: [...KEY, workspaceId, thread ?? null],
    queryFn: () => ipc.threadInstructionsStatus(workspaceId, thread),
    placeholderData: keepPreviousData,
  });
  // Read again when a turn starts: a file added or removed shows from the next turn; the list so far stays meanwhile.
  useEffect(() => {
    if (turn.turnId !== null) void queryClient.invalidateQueries({ queryKey: [...KEY, workspaceId] });
  }, [queryClient, turn.turnId, workspaceId]);
  const save = useMutation({
    mutationFn: (respect: boolean) => ipc.setThreadRespectAgentsMd(workspaceId, conversationId, respect),
    onSettled: () => queryClient.invalidateQueries({ queryKey: [...KEY, workspaceId] }),
  });
  const [pending, setPending] = usePendingRespect(workspaceId);
  const toggle = (next: boolean) => (thread === undefined ? setPending(next) : save.mutate(next));
  const files = listed(status.data?.files ?? [], turn.view.projectInstructions?.files);
  return { thread, status, save, toggle, files, ...switchState(thread === undefined ? pending : undefined, status, save) };
}

export function ProjectInstructionsSection(props: ProjectInstructionsSectionProps): ReactNode {
  const { thread, status, save, respect, disabled, toggle, files } = useInstructions(props);
  if (status.data === undefined && files.length === 0) return null;
  return (
    <Box
      data-testid="project-instructions"
      sx={(theme: Theme) => ({ display: 'flex', flexDirection: 'column', gap: 0.5, paddingX: 2, paddingY: 1, borderTop: `0.0625rem solid ${theme.vars.palette.divider}` })}
    >
      <Typography variant="labelSmall" color="text.secondary">{t('workspace.instructions.title', 'Project instructions')}</Typography>
      {files.length === 0 ? (
        <Typography variant="bodySmall" color="text.secondary" data-testid="project-instructions-none">
          {t('workspace.instructions.none', 'No AGENTS.md in this folder.')}
        </Typography>
      ) : (
        <>
          {files.map((file) => (
            <Typography key={file} variant="bodySmall" data-testid="project-instructions-file" sx={{ fontFamily: 'monospace', overflowWrap: 'anywhere' }}>
              {file}
            </Typography>
          ))}
          <FormControlLabel
            data-testid="respect-agents-md"
            control={<Switch size="small" checked={respect} disabled={disabled} onChange={(_event, next) => toggle(next)} />}
            label={<Typography variant="bodySmall">{t('workspace.instructions.respect', 'Respect AGENTS.md')}</Typography>}
          />
          {!respect && (
            <Typography variant="bodySmall" color="warning.main" component="output" data-testid="agents-md-ignored">
              {t('workspace.instructions.ignored', 'AGENTS.md ignored in this thread')}
            </Typography>
          )}
          {thread === undefined && (
            <Typography variant="bodySmall" color="text.secondary">{t('workspace.instructions.beforeFirst', 'Applies to the thread your first message starts.')}</Typography>
          )}
          {save.isError && (
            <Typography variant="bodySmall" color="error" role="alert">{t('workspace.instructions.saveFailed', 'The setting could not be saved. Try again.')}</Typography>
          )}
        </>
      )}
    </Box>
  );
}
