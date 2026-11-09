/**
 * A project's saved teams (ADR-0050 decision 8, issue 1620): the list, with
 * New team, Edit, Delete and Start conversation.
 *
 * Starting a conversation adds the team's agents through the ordinary
 * participant path under the caller's own access, so a team never lets a
 * person reach an agent they could not add by hand; a refusal is shown in
 * the server's words. What the caller may do comes from the project's
 * `models.teams.*` permissions.
 */
import type { ReactNode } from 'react';
import { useState } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';

import type { AgentTeam, CreatedConversation } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { DeleteEntityModal } from '@/shared/ui/DeleteEntityModal';

import { teamErrorMessage } from '../lib/teamDraft';
import { useAgentTeamList, useAgentTeamWrites, useTeamAbilities } from '../model/useAgentTeams';
import { TeamEditDialog } from './TeamEditDialog';
import { TeamRow } from './TeamRow';

export interface TeamsPanelProps {
  readonly projectId: string | undefined;
  /** Called with the conversation a start made; the page opens it. */
  readonly onStarted: (conversation: CreatedConversation) => void;
}

type Editing = { readonly team: AgentTeam | undefined } | undefined;

export function TeamsPanel({ projectId, onStarted }: TeamsPanelProps): ReactNode {
  const abilities = useTeamAbilities(projectId);
  if (projectId === undefined) return null;
  if (!abilities.canList) {
    return (
      <Typography variant="bodyMedium" color="text.secondary" data-testid="agent-teams-forbidden">
        {t('features.agentTeams.forbidden', 'You do not have access to this project\'s teams.')}
      </Typography>
    );
  }
  return <TeamsList projectId={projectId} abilities={abilities} onStarted={onStarted} />;
}

function Alert({ testId, children }: { readonly testId: string; readonly children: ReactNode }): ReactNode {
  return (
    <Typography role="alert" variant="bodySmall" color="error" data-testid={testId}>
      {children}
    </Typography>
  );
}

function TeamsList({
  projectId,
  abilities,
  onStarted,
}: {
  readonly projectId: string;
  readonly abilities: ReturnType<typeof useTeamAbilities>;
  readonly onStarted: TeamsPanelProps['onStarted'];
}): ReactNode {
  const list = useAgentTeamList(projectId, '');
  const writes = useAgentTeamWrites(projectId);
  const [editing, setEditing] = useState<Editing>(undefined);
  const [deleting, setDeleting] = useState<AgentTeam | undefined>(undefined);
  const [deleteError, setDeleteError] = useState<string | undefined>(undefined);
  const [startError, setStartError] = useState<string | undefined>(undefined);

  const start = (team: AgentTeam): void => {
    setStartError(undefined);
    writes.start.mutate(team.id, {
      onSuccess: (conversation) => onStarted(conversation),
      onError: (cause) => setStartError(teamErrorMessage(cause, t('features.agentTeams.startFailed', 'The conversation was not started. Try again.'))),
    });
  };

  const confirmDelete = (): void => {
    if (deleting === undefined) return;
    setDeleteError(undefined);
    writes.remove.mutate(deleting.id, {
      onSuccess: () => setDeleting(undefined),
      onError: (cause) => setDeleteError(teamErrorMessage(cause, t('features.agentTeams.deleteFailed', 'The team was not deleted. Try again.'))),
    });
  };

  const teams = list.data?.rows ?? [];
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: '1rem' }} data-testid="agent-teams-panel">
      <Box sx={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: '1rem' }}>
        <Typography variant="bodyMedium" color="text.secondary">
          {t('features.agentTeams.intro', 'A team is a saved set of agents with its lead. Start a conversation with all of them at once.')}
        </Typography>
        {abilities.canCreate && (
          <BaseBtn variant="contained" onClick={() => setEditing({ team: undefined })} data-testid="agent-teams-create">
            {t('features.agentTeams.create', 'New team')}
          </BaseBtn>
        )}
      </Box>
      {startError !== undefined && <Alert testId="agent-teams-start-error">{startError}</Alert>}
      {list.isError && (
        <Alert testId="agent-teams-load-error">{teamErrorMessage(list.error, t('features.agentTeams.loadFailed', 'The teams could not be loaded.'))}</Alert>
      )}
      {list.isSuccess && teams.length === 0 && (
        <Typography variant="bodyMedium" color="text.secondary" data-testid="agent-teams-empty">
          {t('features.agentTeams.empty', 'No teams yet. Save one here, or from a conversation\'s participants.')}
        </Typography>
      )}
      {teams.map((team) => (
        <TeamRow
          key={team.id}
          team={team}
          starting={writes.start.isPending}
          onStart={() => start(team)}
          onEdit={abilities.canUpdate ? () => setEditing({ team }) : undefined}
          onDelete={abilities.canDelete ? () => { setDeleteError(undefined); setDeleting(team); } : undefined}
        />
      ))}
      <TeamEditDialog projectId={projectId} open={editing !== undefined} team={editing?.team} onClose={() => setEditing(undefined)} />
      <DeleteEntityModal
        open={deleting !== undefined}
        name={deleting?.name ?? ''}
        onClose={() => setDeleting(undefined)}
        onConfirm={confirmDelete}
        confirming={writes.remove.isPending}
        {...(deleteError !== undefined ? { errorMessage: deleteError } : {})}
      />
    </Box>
  );
}
