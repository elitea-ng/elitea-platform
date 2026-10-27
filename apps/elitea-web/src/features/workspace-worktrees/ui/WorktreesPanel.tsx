/**
 * A git folder's parallel tasks in the sidebar (#1235): each task worktree is
 * a thread of the folder, drawn by the sidebar (`renderEntry`: its thread's
 * title, a branch glyph, how many commits it is ahead and how many paths are
 * uncommitted) with this panel's actions on hover: Diff, Merge back, Remove.
 * A task worktree is started from a new thread's composer ("Work in › New
 * worktree"), not from here.
 *
 * Remove asks the person only when the host refuses it for uncommitted work
 * (`useWorktrees`). Renders nothing outside the desktop host.
 */
import { useState, type ReactNode } from 'react';

import CallMergeOutlinedIcon from '@mui/icons-material/CallMergeOutlined';
import DeleteOutlineOutlinedIcon from '@mui/icons-material/DeleteOutlineOutlined';
import DifferenceOutlinedIcon from '@mui/icons-material/DifferenceOutlined';
import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import Tooltip from '@mui/material/Tooltip';

import type { WorktreeEntry, WorktreeIpc } from '@/shared/desktop/worktreeIpc';
import { t } from '@/shared/i18n';

import { useWorktreeIpc } from '../model/ipcContext';
import { useWorktrees } from '../model/useWorktrees';
import { MergeBackDialog } from './MergeBackDialog';
import { RemoveWorktreeDialog } from './RemoveWorktreeDialog';
import { WorktreeDiffDialog } from './WorktreeDiffDialog';

/** One task as the sidebar draws it: `counts` is its quiet status, `actions` its hover buttons. */
export interface WorktreeRowParts {
  entry: WorktreeEntry;
  counts: string;
  actions: ReactNode;
}

export interface WorktreesPanelProps {
  /** The parent (main) folder's workspace id: what every `worktree_*` command takes. */
  workspaceId: string;
  workspaceName: string;
  renderEntry: (parts: WorktreeRowParts) => ReactNode;
  /** The host client; the provided (`WorktreeIpcProvider`) or Tauri one when not given (tests pass a fake). */
  ipc?: WorktreeIpc;
}

const DIFF = 'diff';
const MERGE = 'merge';

type OpenDialog = { kind: typeof DIFF | typeof MERGE; entry: WorktreeEntry } | null;

/** "2 ahead · 1 uncommitted"; nothing for a task with neither. */
function counts(entry: WorktreeEntry): string {
  if (entry.missing) return t('workspace.worktree.missing', 'Folder missing');
  const parts: string[] = [];
  if (entry.ahead > 0) parts.push(t('workspace.worktree.ahead', '{{n}} ahead', { n: entry.ahead }));
  if (entry.dirty > 0) parts.push(t('workspace.worktree.dirty', '{{n}} uncommitted', { n: entry.dirty }));
  return parts.join(' · ');
}

interface EntryActionsProps {
  entry: WorktreeEntry;
  busy: boolean;
  onDiff: () => void;
  onMerge: () => void;
  onRemove: () => void;
}

function EntryActions({ entry, busy, onDiff, onMerge, onRemove }: EntryActionsProps): React.JSX.Element {
  const action = (label: string, icon: React.JSX.Element, run: () => void, disabled = false): React.JSX.Element => (
    <Tooltip title={label}>
      <span>
        <IconButton
          size="small"
          aria-label={label}
          disabled={disabled || busy}
          onClick={(event) => {
            // The row under the buttons opens the task.
            event.stopPropagation();
            run();
          }}
        >
          {icon}
        </IconButton>
      </span>
    </Tooltip>
  );
  return (
    <>
      {action(t('workspace.worktree.diff', 'Diff'), <DifferenceOutlinedIcon fontSize="inherit" />, onDiff, entry.missing)}
      {action(t('workspace.worktree.mergeBack', 'Merge back'), <CallMergeOutlinedIcon fontSize="inherit" />, onMerge, entry.missing)}
      {action(t('workspace.worktree.remove', 'Remove'), <DeleteOutlineOutlinedIcon fontSize="inherit" />, onRemove)}
    </>
  );
}

function Panel({ ipc, workspaceId, workspaceName, renderEntry }: Required<WorktreesPanelProps>): React.JSX.Element | null {
  const worktrees = useWorktrees(ipc, workspaceId);
  const [dialog, setDialog] = useState<OpenDialog>(null);
  const close = (): void => setDialog(null);
  return (
    <Box data-testid="worktrees-panel">
      <Box component="ul" sx={{ margin: 0, padding: 0, listStyle: 'none' }} aria-label={t('workspace.worktree.listLabel', 'Parallel tasks in {{name}}', { name: workspaceName })}>
        {worktrees.entries.map((entry) => (
          <li key={entry.thread_uuid} data-testid="worktree-row">
            {renderEntry({
              entry,
              counts: counts(entry),
              actions: (
                <EntryActions
                  entry={entry}
                  busy={worktrees.removing}
                  onDiff={() => setDialog({ kind: DIFF, entry })}
                  onMerge={() => setDialog({ kind: MERGE, entry })}
                  onRemove={() => worktrees.remove(entry)}
                />
              ),
            })}
          </li>
        ))}
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

export function WorktreesPanel({ workspaceId, workspaceName, renderEntry, ipc: provided }: WorktreesPanelProps): React.JSX.Element | null {
  const fromContext = useWorktreeIpc();
  const ipc = provided ?? fromContext;
  if (ipc === undefined) return null;
  return <Panel ipc={ipc} workspaceId={workspaceId} workspaceName={workspaceName} renderEntry={renderEntry} />;
}
