/**
 * The messages that wait for the running turn (`useSteering`), above the composer: each with Edit and Remove
 * until the agent takes it at its next safe point. Once taken, it shows in the transcript as the person's message.
 */
import { useState } from 'react';

import CloseIcon from '@mui/icons-material/Close';
import EditOutlinedIcon from '@mui/icons-material/EditOutlined';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import IconButton from '@mui/material/IconButton';
import type { Theme } from '@mui/material/styles';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import type { PendingSteer } from './useSteering';

export interface SteeringQueueProps {
  pending: readonly PendingSteer[];
  error: string | null;
  onEdit: (steerId: string, text: string) => Promise<void>;
  onRemove: (steerId: string) => Promise<void>;
}

function PendingEditor({ steer, onSave, onClose }: { steer: PendingSteer; onSave: (text: string) => Promise<void>; onClose: () => void }): React.JSX.Element {
  const [draft, setDraft] = useState(steer.text);
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: 0.5, flex: 1 }}>
      <TextField
        size="small"
        multiline
        maxRows={6}
        value={draft}
        onChange={(event) => setDraft(event.target.value)}
        slotProps={{ htmlInput: { 'aria-label': t('workspace.steer.editLabel', 'Queued message') } }}
      />
      <Box sx={{ display: 'flex', gap: 1, justifyContent: 'flex-end' }}>
        <Button size="small" onClick={onClose}>
          {t('workspace.steer.cancelEdit', 'Cancel')}
        </Button>
        <Button size="small" variant="contained" disabled={draft.trim() === ''} onClick={() => void onSave(draft).then(onClose)}>
          {t('workspace.steer.save', 'Save')}
        </Button>
      </Box>
    </Box>
  );
}

function PendingRow({ steer, onEdit, onRemove }: { steer: PendingSteer } & Pick<SteeringQueueProps, 'onEdit' | 'onRemove'>): React.JSX.Element {
  const [editing, setEditing] = useState(false);
  return (
    <Box
      data-testid="steer-pending"
      sx={(theme: Theme) => ({
        display: 'flex',
        alignItems: 'flex-start',
        gap: 1,
        paddingX: 1.5,
        paddingY: 1,
        borderRadius: theme.vars.shape.radiusMd,
        border: `1px dashed ${theme.vars.palette.divider}`,
      })}
    >
      {editing ? (
        <PendingEditor steer={steer} onSave={(text) => onEdit(steer.steerId, text)} onClose={() => setEditing(false)} />
      ) : (
        <>
          <Typography variant="bodyMedium" sx={{ whiteSpace: 'pre-wrap', flex: 1, minWidth: 0 }}>
            {steer.text}
          </Typography>
          <IconButton size="small" aria-label={t('workspace.steer.edit', 'Edit queued message')} onClick={() => setEditing(true)}>
            <EditOutlinedIcon fontSize="small" />
          </IconButton>
          <IconButton size="small" aria-label={t('workspace.steer.remove', 'Remove queued message')} onClick={() => void onRemove(steer.steerId)}>
            <CloseIcon fontSize="small" />
          </IconButton>
        </>
      )}
    </Box>
  );
}

export function SteeringQueue({ pending, error, onEdit, onRemove }: SteeringQueueProps): React.JSX.Element | null {
  if (pending.length === 0 && error === null) return null;
  return (
    <Box data-testid="steering-queue" sx={{ display: 'flex', flexDirection: 'column', gap: 0.75 }}>
      {pending.length > 0 && (
        <Typography variant="bodySmall" component="p" sx={(theme: Theme) => ({ color: theme.vars.palette.text.secondary })}>
          {t('workspace.steer.waiting', 'The agent reads these after its current step:')}
        </Typography>
      )}
      {pending.map((steer) => (
        <PendingRow key={steer.steerId} steer={steer} onEdit={onEdit} onRemove={onRemove} />
      ))}
      {error !== null && (
        <Typography variant="bodySmall" role="alert" data-testid="steer-error" sx={(theme: Theme) => ({ color: theme.vars.palette.error.main })}>
          {error}
        </Typography>
      )}
    </Box>
  );
}
