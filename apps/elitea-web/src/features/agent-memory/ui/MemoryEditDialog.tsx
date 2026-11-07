/**
 * Edit one memory entry's text (and its kind, for a manager). Used by the
 * Memory tab and by the chat's "remembered" chip.
 *
 * A person who is not a manager may only correct the text of an entry about
 * themself; the server refuses any other change (403
 * `memory_change_forbidden`), so the kind control is not offered to them. A
 * refusal stays in the dialog, in the server's own words.
 */
import type { ChangeEvent, ReactNode } from 'react';
import { useState } from 'react';

import Box from '@mui/material/Box';
import MenuItem from '@mui/material/MenuItem';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import type { AgentMemoryEntry, AgentMemoryUpdate } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { BaseModal } from '@/shared/ui/BaseModal';

import { memoryErrorMessage } from '../lib/memoryFilters';
import { useAgentMemoryWrites } from '../model/useAgentMemory';

/** The server's bound (`agentmemory.MaxContentBytes`). */
const MAX_CONTENT_BYTES = 2048;
const EDITABLE_KINDS = ['fact', 'preference', 'episode'] as const;
type EditableKind = (typeof EDITABLE_KINDS)[number];

export interface MemoryEditDialogProps {
  readonly projectId: string;
  readonly applicationId: number;
  /** The entry being edited; `undefined` closes the dialog. */
  readonly entry: { readonly id: number; readonly content: string; readonly kind?: string | undefined } | undefined;
  /** The caller may change the kind (a manager). */
  readonly canChangeKind: boolean;
  readonly onClose: () => void;
  /** Called with the saved entry before the dialog closes. */
  readonly onSaved?: ((entry: AgentMemoryEntry) => void) | undefined;
}

function byteLength(text: string): number {
  return new TextEncoder().encode(text).length;
}

function isEditableKind(kind: string | undefined): kind is EditableKind {
  return (EDITABLE_KINDS as readonly string[]).includes(kind ?? '');
}

export function MemoryEditDialog({ projectId, applicationId, entry, canChangeKind, onClose, onSaved }: MemoryEditDialogProps): ReactNode {
  if (entry === undefined) return null;
  return (
    <MemoryEditForm
      key={entry.id}
      projectId={projectId}
      applicationId={applicationId}
      entry={entry}
      canChangeKind={canChangeKind}
      onClose={onClose}
      onSaved={onSaved}
    />
  );
}

function MemoryEditForm({
  projectId,
  applicationId,
  entry,
  canChangeKind,
  onClose,
  onSaved,
}: Omit<MemoryEditDialogProps, 'entry'> & { readonly entry: NonNullable<MemoryEditDialogProps['entry']> }): ReactNode {
  const { update } = useAgentMemoryWrites(projectId, applicationId);
  const [content, setContent] = useState(entry.content);
  const [kind, setKind] = useState<string>(entry.kind ?? '');
  const [error, setError] = useState<string | undefined>(undefined);
  const trimmed = content.trim();
  const tooLong = byteLength(trimmed) > MAX_CONTENT_BYTES;
  const kindChanged = canChangeKind && isEditableKind(kind) && kind !== entry.kind;
  const changed = trimmed !== entry.content.trim() || kindChanged;

  const save = (): void => {
    if (trimmed === '' || tooLong || !changed) return;
    const change: AgentMemoryUpdate = {
      ...(trimmed !== entry.content.trim() ? { content: trimmed } : {}),
      ...(kindChanged && isEditableKind(kind) ? { kind } : {}),
    };
    setError(undefined);
    update.mutate(
      { entryId: entry.id, change },
      {
        onSuccess: (saved) => {
          onSaved?.(saved);
          onClose();
        },
        onError: (cause) => setError(memoryErrorMessage(cause, t('features.agentMemory.edit.failed', 'The memory was not saved. Try again.'))),
      },
    );
  };

  return (
    <BaseModal
      open
      data-testid="agent-memory-edit-dialog"
      title={t('features.agentMemory.edit.title', 'Edit memory')}
      onClose={onClose}
      onConfirm={save}
      actions={{
        confirmText: t('features.agentMemory.edit.save', 'Save'),
        confirming: update.isPending || trimmed === '' || tooLong || !changed,
      }}
      content={
        <Box sx={{ display: 'flex', flexDirection: 'column', gap: '1rem', minWidth: '28rem' }}>
          <TextField
            label={t('features.agentMemory.edit.content', 'What the agent remembers')}
            value={content}
            onChange={(event: ChangeEvent<HTMLInputElement>) => setContent(event.target.value)}
            multiline
            minRows={3}
            error={tooLong}
            helperText={tooLong ? t('features.agentMemory.edit.tooLong', 'A memory is at most 2,048 bytes.') : undefined}
            slotProps={{ htmlInput: { 'data-testid': 'agent-memory-edit-content' } }}
          />
          {canChangeKind && isEditableKind(entry.kind) && (
            <TextField
              select
              label={t('features.agentMemory.filters.kind', 'Kind')}
              value={kind}
              onChange={(event: ChangeEvent<HTMLInputElement>) => setKind(event.target.value)}
              slotProps={{ htmlInput: { 'data-testid': 'agent-memory-edit-kind' } }}
            >
              {EDITABLE_KINDS.map((value) => (
                <MenuItem key={value} value={value}>
                  {kindLabel(value)}
                </MenuItem>
              ))}
            </TextField>
          )}
          {error !== undefined && (
            <Typography role="alert" variant="bodySmall" color="error" data-testid="agent-memory-edit-error">
              {error}
            </Typography>
          )}
        </Box>
      }
    />
  );
}

export function kindLabel(kind: string): string {
  switch (kind) {
    case 'fact':
      return t('features.agentMemory.kind.fact', 'Fact');
    case 'preference':
      return t('features.agentMemory.kind.preference', 'Preference');
    case 'episode':
      return t('features.agentMemory.kind.episode', 'Episode');
    case 'cursor':
      return t('features.agentMemory.kind.cursor', 'Heartbeat state');
    default:
      return kind;
  }
}

export function statusLabel(status: string): string {
  switch (status) {
    case 'pending':
      return t('features.agentMemory.status.pending', 'Waiting for approval');
    case 'active':
      return t('features.agentMemory.status.active', 'Active');
    case 'archived':
      return t('features.agentMemory.status.archived', 'Archived');
    default:
      return status;
  }
}
