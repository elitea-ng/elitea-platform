/**
 * "Save as team" from a conversation (issue 1620): names the team; the
 * server reads the conversation's agents, their versions and its lead.
 */
import type { ChangeEvent, ReactNode } from 'react';
import { useState } from 'react';

import Box from '@mui/material/Box';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import type { AgentTeam } from '@/shared/api/generated/model';
import { serverErrorMessage } from '@/shared/api/serverErrorMessage';
import { t } from '@/shared/i18n';
import { BaseModal } from '@/shared/ui/BaseModal';

import { MAX_TEAM_NAME } from '../lib/teamDraft';
import { useAgentTeamWrites } from '../model/useAgentTeams';

export interface SaveAsTeamDialogProps {
  readonly projectId: string;
  readonly conversationId: string;
  /** The suggested name: the conversation's own. */
  readonly defaultName: string;
  readonly open: boolean;
  readonly onClose: () => void;
  readonly onSaved?: ((team: AgentTeam) => void) | undefined;
}

export function SaveAsTeamDialog({ open, ...rest }: SaveAsTeamDialogProps): ReactNode {
  if (!open) return null;
  return <SaveAsTeamForm {...rest} />;
}

function SaveAsTeamForm({ projectId, conversationId, defaultName, onClose, onSaved }: Omit<SaveAsTeamDialogProps, 'open'>): ReactNode {
  const { fromConversation } = useAgentTeamWrites(projectId);
  const [name, setName] = useState(Array.from(defaultName).slice(0, MAX_TEAM_NAME).join(''));
  const [description, setDescription] = useState('');
  const [error, setError] = useState<string | undefined>(undefined);
  const blank = name.trim() === '';

  const save = (): void => {
    if (blank || fromConversation.isPending) return;
    setError(undefined);
    fromConversation.mutate(
      { conversationId, name, description },
      {
        onSuccess: (team) => {
          onSaved?.(team);
          onClose();
        },
        onError: (cause) => setError(serverErrorMessage(cause, t('features.agentTeams.saveAs.failed', 'The team was not saved. Try again.'))),
      },
    );
  };

  return (
    <BaseModal
      open
      data-testid="agent-team-save-as-dialog"
      title={t('features.agentTeams.saveAs.title', 'Save as team')}
      onClose={onClose}
      onConfirm={save}
      actions={{ confirmText: t('features.agentTeams.saveAs.save', 'Save'), confirming: blank || fromConversation.isPending }}
      content={
        <Box sx={{ display: 'flex', flexDirection: 'column', gap: '1rem', minWidth: '26rem' }}>
          <Typography variant="bodySmall" color="text.secondary">
            {t('features.agentTeams.saveAs.hint', 'Saves this conversation\'s agents, the version each one runs and its lead.')}
          </Typography>
          <TextField
            label={t('features.agentTeams.edit.name', 'Name')}
            value={name}
            onChange={(event: ChangeEvent<HTMLInputElement>) => setName(event.target.value)}
            slotProps={{ htmlInput: { 'data-testid': 'agent-team-save-as-name', maxLength: MAX_TEAM_NAME } }}
          />
          <TextField
            label={t('features.agentTeams.edit.description', 'Description')}
            value={description}
            onChange={(event: ChangeEvent<HTMLInputElement>) => setDescription(event.target.value)}
            multiline
            minRows={2}
            slotProps={{ htmlInput: { 'data-testid': 'agent-team-save-as-description' } }}
          />
          {error !== undefined && (
            <Typography role="alert" variant="bodySmall" color="error" data-testid="agent-team-save-as-error">
              {error}
            </Typography>
          )}
        </Box>
      }
    />
  );
}
