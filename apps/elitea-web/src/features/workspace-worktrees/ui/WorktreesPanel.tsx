/**
 * A git folder's parallel tasks in the sidebar (#1235): each task worktree on
 * one line, its branch as a chip, how many commits it is ahead and how many
 * paths are uncommitted, and its actions: Diff, Merge back, Remove. Under them
 * "New parallel task" makes a worktree for a new thread (a fresh UUID) on its
 * own branch from the folder's HEAD.
 *
 * Remove asks the person only when the host refuses it for uncommitted work
 * (`useWorktrees`). Renders nothing outside the desktop host.
 */
import { useMemo, useState } from 'react';

import AltRouteOutlinedIcon from '@mui/icons-material/AltRouteOutlined';
import CallMergeOutlinedIcon from '@mui/icons-material/CallMergeOutlined';
import DeleteOutlineOutlinedIcon from '@mui/icons-material/DeleteOutlineOutlined';
import DifferenceOutlinedIcon from '@mui/icons-material/DifferenceOutlined';
import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import ButtonBase from '@mui/material/ButtonBase';
import Chip from '@mui/material/Chip';
import IconButton from '@mui/material/IconButton';
import type { Theme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import { tauriWorktreeIpc, type WorktreeEntry, type WorktreeIpc } from '@/shared/desktop/worktreeIpc';
import { t } from '@/shared/i18n';

import { useWorktrees } from '../model/useWorktrees';
import { MergeBackDialog } from './MergeBackDialog';
import { RemoveWorktreeDialog } from './RemoveWorktreeDialog';
import { WorktreeDiffDialog } from './WorktreeDiffDialog';

export interface WorktreesPanelProps {
  /** The parent (main) folder's workspace id: what every `worktree_*` command takes. */
  workspaceId: string;
  workspaceName: string;
  /** The host client; the Tauri one when not given (tests pass a fake). */
  ipc?: WorktreeIpc;
}

/** Lines up with the folder's thread rows (the sidebar's child indent). */
const ROW_INDENT = 4;

const DIFF = 'diff';
const MERGE = 'merge';

type OpenDialog = { kind: typeof DIFF | typeof MERGE; entry: WorktreeEntry } | null;

function counts(entry: WorktreeEntry): string {
  if (entry.missing) return t('workspace.worktree.missing', 'Folder missing');
  const parts = [t('workspace.worktree.ahead', '{{n}} ahead', { n: entry.ahead })];
  if (entry.dirty > 0) parts.push(t('workspace.worktree.dirty', '{{n}} uncommitted', { n: entry.dirty }));
  return parts.join(' · ');
}

interface EntryRowProps {
  entry: WorktreeEntry;
  busy: boolean;
  onDiff: () => void;
  onMerge: () => void;
  onRemove: () => void;
}

function NewTaskRow({ creating, onCreate }: { creating: boolean; onCreate: () => void }): React.JSX.Element {
  return (
    <ButtonBase
      onClick={onCreate}
      disabled={creating}
      sx={(theme: Theme) => ({
        width: '100%',
        justifyContent: 'flex-start',
        gap: 1,
        paddingY: 0.25,
        paddingLeft: ROW_INDENT,
        paddingRight: 1,
        minHeight: '1.75rem',
        borderRadius: theme.vars.shape.radiusSm,
        color: theme.vars.palette.text.metrics,
        '&:hover': { background: theme.vars.palette.background.button.drawerMenu.hover },
      })}
    >
      <AltRouteOutlinedIcon fontSize="inherit" />
      <Typography variant="bodySmall" component="span">
        {creating ? t('workspace.worktree.creating', 'Making a parallel task…') : t('workspace.worktree.new', 'New parallel task')}
      </Typography>
    </ButtonBase>
  );
}

function EntryRow({ entry, busy, onDiff, onMerge, onRemove }: EntryRowProps): React.JSX.Element {
  const action = (label: string, icon: React.JSX.Element, run: () => void, disabled = false): React.JSX.Element => (
    <Tooltip title={label}>
      <span>
        <IconButton size="small" aria-label={label} disabled={disabled || busy} onClick={run}>
          {icon}
        </IconButton>
      </span>
    </Tooltip>
  );
  return (
    <Box
      data-testid="worktree-row"
      title={entry.path}
      sx={{ display: 'flex', alignItems: 'center', gap: 1, paddingLeft: ROW_INDENT, paddingRight: 0.5, minHeight: '1.75rem', minWidth: 0 }}
    >
      <Chip size="small" variant="outlined" label={entry.branch} sx={{ minWidth: 0, maxWidth: '50%' }} />
      <Typography
        variant="bodySmall"
        component="span"
        data-testid="worktree-counts"
        sx={(theme: Theme) => ({
          flex: 1,
          minWidth: 0,
          overflow: 'hidden',
          textOverflow: 'ellipsis',
          whiteSpace: 'nowrap',
          color: entry.missing ? theme.vars.palette.warning.main : theme.vars.palette.text.metrics,
        })}
      >
        {counts(entry)}
      </Typography>
      {action(t('workspace.worktree.diff', 'Diff'), <DifferenceOutlinedIcon fontSize="inherit" />, onDiff, entry.missing)}
      {action(t('workspace.worktree.mergeBack', 'Merge back'), <CallMergeOutlinedIcon fontSize="inherit" />, onMerge, entry.missing)}
      {action(t('workspace.worktree.remove', 'Remove'), <DeleteOutlineOutlinedIcon fontSize="inherit" />, onRemove)}
    </Box>
  );
}

function Panel({ ipc, workspaceId, workspaceName }: { ipc: WorktreeIpc; workspaceId: string; workspaceName: string }): React.JSX.Element {
  const worktrees = useWorktrees(ipc, workspaceId);
  const [dialog, setDialog] = useState<OpenDialog>(null);
  const close = (): void => setDialog(null);
  return (
    <Box data-testid="worktrees-panel">
      <Box component="ul" sx={{ margin: 0, padding: 0, listStyle: 'none' }} aria-label={t('workspace.worktree.listLabel', 'Parallel tasks in {{name}}', { name: workspaceName })}>
        {worktrees.entries.map((entry) => (
          <li key={entry.thread_uuid}>
            <EntryRow
              entry={entry}
              busy={worktrees.removing}
              onDiff={() => setDialog({ kind: DIFF, entry })}
              onMerge={() => setDialog({ kind: MERGE, entry })}
              onRemove={() => worktrees.remove(entry)}
            />
          </li>
        ))}
        <li>
          <NewTaskRow creating={worktrees.creating} onCreate={worktrees.create} />
        </li>
      </Box>
      {worktrees.error !== null && (
        <Alert severity="error" onClose={worktrees.clearError} sx={{ marginX: 1 }}>
          {worktrees.error}
        </Alert>
      )}
      {dialog?.kind === DIFF && <WorktreeDiffDialog ipc={ipc} workspaceId={workspaceId} entry={dialog.entry} onClose={close} />}
      {dialog?.kind === MERGE && <MergeBackDialog ipc={ipc} workspaceId={workspaceId} entry={dialog.entry} onClose={close} onChanged={() => void worktrees.refresh()} />}
      {worktrees.confirming !== null && (
        <RemoveWorktreeDialog
          entry={worktrees.confirming}
          busy={worktrees.removing}
          error={worktrees.forcedFailure}
          onConfirm={worktrees.confirmRemove}
          onClose={worktrees.cancelRemove}
        />
      )}
    </Box>
  );
}

export function WorktreesPanel({ workspaceId, workspaceName, ipc: provided }: WorktreesPanelProps): React.JSX.Element | null {
  const ipc = useMemo(() => provided ?? tauriWorktreeIpc(), [provided]);
  if (ipc === undefined) return null;
  return <Panel ipc={ipc} workspaceId={workspaceId} workspaceName={workspaceName} />;
}
