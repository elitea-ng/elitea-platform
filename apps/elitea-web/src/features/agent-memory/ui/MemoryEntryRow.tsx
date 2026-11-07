/**
 * One memory entry in the Memory tab: its text, its kind and status, who it
 * is about and who wrote it, and the actions the caller may take on it.
 *
 * WHICH ACTIONS. The list's `can_*` flags are the server's answer for this
 * caller and this agent; a row offers only what that answer allows, plus what
 * anyone may do with an entry about themself (correct its text, delete it).
 * Approve and reject appear on a pending entry only for an approver.
 */
import type { ReactNode } from 'react';

import ArchiveOutlinedIcon from '@mui/icons-material/ArchiveOutlined';
import CheckIcon from '@mui/icons-material/Check';
import CloseIcon from '@mui/icons-material/Close';
import DeleteOutlineIcon from '@mui/icons-material/DeleteOutlined';
import EditOutlinedIcon from '@mui/icons-material/EditOutlined';
import HistoryIcon from '@mui/icons-material/History';
import PushPinIcon from '@mui/icons-material/PushPin';
import PushPinOutlinedIcon from '@mui/icons-material/PushPinOutlined';
import UnarchiveOutlinedIcon from '@mui/icons-material/UnarchiveOutlined';
import Box from '@mui/material/Box';
import Chip from '@mui/material/Chip';
import IconButton from '@mui/material/IconButton';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import type { AgentMemoryEntry } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import { kindLabel, statusLabel } from './MemoryEditDialog';
import { writerLabel } from './MemoryHistoryDialog';

export interface MemoryRowAbilities {
  readonly userId: number | undefined;
  readonly canManage: boolean;
  readonly canDelete: boolean;
  readonly canApprove: boolean;
}

export type MemoryRowAction = 'approve' | 'reject' | 'edit' | 'pin' | 'unpin' | 'archive' | 'restore' | 'delete' | 'history';

export interface MemoryEntryRowProps {
  readonly entry: AgentMemoryEntry;
  readonly names: ReadonlyMap<number, string>;
  readonly abilities: MemoryRowAbilities;
  readonly busy: boolean;
  readonly onAction: (action: MemoryRowAction, entry: AgentMemoryEntry) => void;
}

const rowSx: SxProps<Theme> = { display: 'flex', gap: '1rem', alignItems: 'flex-start', padding: '0.75rem 0', borderBottom: 1, borderColor: 'divider' };
const bodySx: SxProps<Theme> = { flex: 1, minWidth: 0, display: 'flex', flexDirection: 'column', gap: '0.25rem' };
const chipsSx: SxProps<Theme> = { display: 'flex', flexWrap: 'wrap', gap: '0.25rem', alignItems: 'center' };

/** The actions this caller may take on this entry, in the order they are shown. */
export function rowActions(entry: AgentMemoryEntry, abilities: MemoryRowAbilities): readonly MemoryRowAction[] {
  const own = entry.subject_user_id !== null && entry.subject_user_id === abilities.userId;
  const actions: MemoryRowAction[] = [];
  if (entry.status === 'pending' && abilities.canApprove) actions.push('approve', 'reject');
  if (abilities.canManage || own) actions.push('edit');
  if (abilities.canManage) {
    actions.push(entry.pinned ? 'unpin' : 'pin');
    if (entry.status === 'active') actions.push('archive');
    if (entry.status === 'archived') actions.push('restore');
  }
  actions.push('history');
  if (abilities.canDelete || own) actions.push('delete');
  return actions;
}

function actionView(action: MemoryRowAction): { readonly label: string; readonly icon: ReactNode } {
  switch (action) {
    case 'approve':
      return { label: t('features.agentMemory.action.approve', 'Approve'), icon: <CheckIcon fontSize="small" /> };
    case 'reject':
      return { label: t('features.agentMemory.action.reject', 'Reject'), icon: <CloseIcon fontSize="small" /> };
    case 'edit':
      return { label: t('features.agentMemory.action.edit', 'Edit'), icon: <EditOutlinedIcon fontSize="small" /> };
    case 'pin':
      return { label: t('features.agentMemory.action.pin', 'Pin'), icon: <PushPinOutlinedIcon fontSize="small" /> };
    case 'unpin':
      return { label: t('features.agentMemory.action.unpin', 'Unpin'), icon: <PushPinIcon fontSize="small" /> };
    case 'archive':
      return { label: t('features.agentMemory.action.archive', 'Archive'), icon: <ArchiveOutlinedIcon fontSize="small" /> };
    case 'restore':
      return { label: t('features.agentMemory.action.restore', 'Restore'), icon: <UnarchiveOutlinedIcon fontSize="small" /> };
    case 'history':
      return { label: t('features.agentMemory.action.history', 'History'), icon: <HistoryIcon fontSize="small" /> };
    case 'delete':
      return { label: t('features.agentMemory.action.delete', 'Delete'), icon: <DeleteOutlineIcon fontSize="small" /> };
  }
}

export function MemoryEntryRow({ entry, names, abilities, busy, onAction }: MemoryEntryRowProps): ReactNode {
  const subject = entry.subject_user_id === null ? undefined : (names.get(entry.subject_user_id) ?? t('features.agentMemory.someone', 'a person'));
  return (
    <Box sx={rowSx} data-testid={`agent-memory-row-${entry.id}`} data-status={entry.status}>
      <Box sx={bodySx}>
        <Typography variant="bodyMedium" sx={{ whiteSpace: 'pre-wrap', overflowWrap: 'anywhere' }} data-testid={`agent-memory-content-${entry.id}`}>
          {entry.content}
        </Typography>
        <Box sx={chipsSx}>
          <Chip size="small" variant="outlined" label={kindLabel(entry.kind)} />
          <Chip size="small" variant="outlined" color={entry.status === 'pending' ? 'warning' : 'default'} label={statusLabel(entry.status)} />
          {entry.pinned && <Chip size="small" variant="outlined" label={t('features.agentMemory.pinned', 'Pinned')} />}
          <Typography variant="bodySmall" color="text.secondary">
            {subject === undefined
              ? t('features.agentMemory.row.meta', '{{writer}} · {{date}}', {
                  writer: writerLabel(entry.written_by_kind, entry.written_by_user_id, names),
                  date: new Date(entry.created_at).toLocaleDateString(),
                })
              : t('features.agentMemory.row.metaAbout', 'About {{subject}} · {{writer}} · {{date}}', {
                  subject,
                  writer: writerLabel(entry.written_by_kind, entry.written_by_user_id, names),
                  date: new Date(entry.created_at).toLocaleDateString(),
                })}
          </Typography>
        </Box>
      </Box>
      <Box sx={{ display: 'flex', flexShrink: 0 }}>
        {rowActions(entry, abilities).map((action) => {
          const view = actionView(action);
          return (
            <Tooltip key={action} title={view.label}>
              <span>
                <IconButton
                  size="small"
                  aria-label={view.label}
                  disabled={busy && action !== 'history'}
                  onClick={() => onAction(action, entry)}
                  data-testid={`agent-memory-${action}-${entry.id}`}
                >
                  {view.icon}
                </IconButton>
              </span>
            </Tooltip>
          );
        })}
      </Box>
    </Box>
  );
}
