/**
 * The participants panel's "Save as team" control (issue 1620): shown for a
 * saved conversation with at least one agent, to a caller who may create
 * teams.
 */
import type { ReactNode } from 'react';
import { useState } from 'react';

import IconButton from '@mui/material/IconButton';
import Tooltip from '@mui/material/Tooltip';
import GroupAddOutlinedIcon from '@mui/icons-material/GroupAddOutlined';

import { t } from '@/shared/i18n';

import { agentCount } from '../lib/teamDraft';
import { useTeamAbilities } from '../model/useAgentTeams';
import { SaveAsTeamDialog } from './SaveAsTeamDialog';

export interface SaveAsTeamButtonProps {
  readonly projectId: string | undefined;
  readonly conversation:
    | { readonly id?: string | number; readonly isNew?: boolean; readonly isPlayback?: boolean; readonly name?: string; readonly participants?: readonly unknown[] }
    | undefined;
}

/** A saved, live conversation with at least one agent. */
export function canSaveAsTeam(conversation: SaveAsTeamButtonProps['conversation']): conversation is NonNullable<SaveAsTeamButtonProps['conversation']> & { readonly id: string | number } {
  if (conversation?.id === undefined) return false;
  if (conversation.isNew === true || conversation.isPlayback === true) return false;
  return agentCount(conversation.participants) > 0;
}

export function SaveAsTeamButton({ projectId, conversation }: SaveAsTeamButtonProps): ReactNode {
  const { canCreate } = useTeamAbilities(projectId);
  const [open, setOpen] = useState(false);
  if (projectId === undefined || !canCreate || !canSaveAsTeam(conversation)) return null;
  const label = t('features.agentTeams.saveAs.title', 'Save as team');
  return (
    <>
      <Tooltip title={label}>
        <IconButton size="small" aria-label={label} data-testid="participants-save-as-team" onClick={() => setOpen(true)}>
          <GroupAddOutlinedIcon fontSize="small" />
        </IconButton>
      </Tooltip>
      <SaveAsTeamDialog
        projectId={projectId}
        conversationId={String(conversation.id)}
        defaultName={conversation.name ?? ''}
        open={open}
        onClose={() => setOpen(false)}
      />
    </>
  );
}
