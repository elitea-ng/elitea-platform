/**
 * Every version of one memory entry, newest first (`getAgentMemoryEntry`'s
 * `history`): what it said, its kind and status, whether it was pinned, and
 * who wrote that version — the agent, or a person by name.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import CircularProgress from '@mui/material/CircularProgress';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { BaseModal } from '@/shared/ui/BaseModal';

import { useAgentMemoryEntry } from '../model/useAgentMemory';
import { kindLabel, statusLabel } from './MemoryEditDialog';

export interface MemoryHistoryDialogProps {
  readonly projectId: string;
  readonly applicationId: number;
  /** The entry whose history is shown; `undefined` closes the dialog. */
  readonly entryId: number | undefined;
  /** User id → display name, from the list's `people`. */
  readonly names: ReadonlyMap<number, string>;
  readonly onClose: () => void;
}

export function writerLabel(kind: string, userId: number | null | undefined, names: ReadonlyMap<number, string>): string {
  if (kind === 'agent') return t('features.agentMemory.writer.agent', 'The agent');
  const name = userId === null || userId === undefined ? undefined : names.get(userId);
  return name ?? t('features.agentMemory.writer.person', 'A person');
}

export function MemoryHistoryDialog({ projectId, applicationId, entryId, names, onClose }: MemoryHistoryDialogProps): ReactNode {
  const entry = useAgentMemoryEntry(projectId, applicationId, entryId);
  if (entryId === undefined) return null;
  const history = entry.data?.history ?? [];
  return (
    <BaseModal
      open
      data-testid="agent-memory-history-dialog"
      title={t('features.agentMemory.history.title', 'Memory history')}
      onClose={onClose}
      actions={{ cancelText: t('features.agentMemory.history.close', 'Close') }}
      content={
        <Box sx={{ display: 'flex', flexDirection: 'column', gap: '0.75rem', minWidth: '28rem' }}>
          {entry.isLoading && <CircularProgress size="1.5rem" />}
          {entry.isError && (
            <Typography role="alert" variant="bodySmall" color="error">
              {t('features.agentMemory.history.failed', 'The history could not be loaded.')}
            </Typography>
          )}
          {history.map((version) => (
            <Box
              key={version.version}
              data-testid={`agent-memory-history-version-${version.version}`}
              sx={{ borderBottom: 1, borderColor: 'divider', paddingBottom: '0.5rem' }}
            >
              <Typography variant="bodySmall" color="text.secondary">
                {t('features.agentMemory.history.meta', 'Version {{version}} · {{kind}} · {{status}} · {{writer}} · {{date}}', {
                  version: version.version,
                  kind: kindLabel(version.kind),
                  status: statusLabel(version.status),
                  writer: writerLabel(version.written_by_kind, version.written_by_user_id, names),
                  date: new Date(version.created_at).toLocaleString(),
                })}
                {version.pinned ? ` · ${t('features.agentMemory.pinned', 'Pinned')}` : ''}
              </Typography>
              <Typography variant="bodyMedium" sx={{ whiteSpace: 'pre-wrap', overflowWrap: 'anywhere' }}>
                {version.content}
              </Typography>
            </Box>
          ))}
        </Box>
      }
    />
  );
}
