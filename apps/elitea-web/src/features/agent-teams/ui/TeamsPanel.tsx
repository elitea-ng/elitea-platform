/**
 * A project's saved teams (ADR-0050 decision 8, issue 1620), redesigned with
 * the Agents cards (issue 3134): a header row with the intro, New team and
 * the page's own actions (Import team), then the teams as cards in the same
 * grid, or an empty state that says what a team is.
 *
 * Starting a conversation adds the team's agents through the ordinary
 * participant path under the caller's own access, so a team never lets a
 * person reach an agent they could not add by hand; a refusal is shown in
 * the server's words. What the caller may do comes from the project's
 * `models.teams.*` permissions.
 *
 * New team is CONTROLLED (`creating`): the page's side panel offers it too,
 * and both open the one dialog here.
 */
import type { ReactNode } from 'react';
import { useState } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { Link } from '@tanstack/react-router';

import type { AgentTeam, CreatedConversation } from '@/shared/api/generated/model';
import { serverErrorMessage } from '@/shared/api/serverErrorMessage';
import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { DeleteEntityModal } from '@/shared/ui/DeleteEntityModal';
import { EntityEmptyState } from '@/shared/ui/EntityCardList';

import { useAgentTeamList, useAgentTeamWrites, useTeamAbilities } from '../model/useAgentTeams';
import { useTeamExport } from '../model/useTeamExport';
import { useTeamMemberAvatars } from '../model/useTeamMemberAvatars';
import { useTeamPublish, type PublishedTeam } from '../model/useTeamPublish';
import type { RenderMemberAvatar } from './TeamAvatarStack';
import { TeamCard } from './TeamCard';
import { TeamEditDialog } from './TeamEditDialog';

export interface TeamsPanelProps {
  readonly projectId: string | undefined;
  /** Called with the conversation a start made; the page opens it. */
  readonly onStarted: (conversation: CreatedConversation) => void;
  /** The New team dialog is open (for a new team). */
  readonly creating: boolean;
  readonly onCreatingChange: (creating: boolean) => void;
  /** The page's actions after New team in the header row: Import team. */
  readonly headerActions?: ReactNode;
  /** Draws a member's live avatar (the shared renderer); a member without one gets the monogram. */
  readonly renderMemberAvatar?: RenderMemberAvatar;
}

export function TeamsPanel(props: TeamsPanelProps): ReactNode {
  const abilities = useTeamAbilities(props.projectId);
  if (props.projectId === undefined) return null;
  if (!abilities.canList) {
    return (
      <Box sx={panelSx}>
        <Typography variant="bodyMedium" color="text.secondary" data-testid="agent-teams-forbidden">
          {t('features.agentTeams.forbidden', 'You do not have access to this project\'s teams.')}
        </Typography>
      </Box>
    );
  }
  return <TeamsList {...props} projectId={props.projectId} abilities={abilities} />;
}

function Alert({ testId, children }: { readonly testId: string; readonly children: ReactNode }): ReactNode {
  return (
    <Typography role="alert" variant="bodySmall" color="error" data-testid={testId}>
      {children}
    </Typography>
  );
}

/** The start and export refusals' test ids, in the order the list renders them. */
const ALERT_IDS = ['agent-teams-start-error', 'agent-teams-export-error'] as const;

/** No teams yet: what a team is, and New team for a person who may create one. */
function TeamsEmpty({ canCreate, onCreate }: { readonly canCreate: boolean; readonly onCreate: () => void }): ReactNode {
  return (
    <Box data-testid="agent-teams-empty">
      <EntityEmptyState
        title={t('features.agentTeams.emptyTitle', 'No teams yet')}
        description={t(
          'features.agentTeams.emptyDescription',
          'A team is a saved set of agents with a lead. Start a conversation with all of them at once. Save one here, or from a conversation\'s participants.',
        )}
        art="applications"
        {...(canCreate ? { onCreateClick: onCreate, createLabel: t('features.agentTeams.create', 'New team') } : {})}
      />
    </Box>
  );
}

/** What the last publish did: the unlisted members it named, or the version and a link to the team's listing. */
function PublishNotices({ error, published }: { readonly error: string | undefined; readonly published: PublishedTeam | undefined }): ReactNode {
  if (error !== undefined) return <Alert testId="agent-teams-publish-error">{error}</Alert>;
  if (published === undefined) return null;
  return (
    <Typography component="output" variant="bodySmall" data-testid="agent-teams-published">
      {t('features.agentTeams.published', 'Published {{version}} to the Marketplace.', { version: published.versionName })}{' '}
      <Link to="/marketplace/$kind/$listingId" params={{ kind: 'teams', listingId: published.listingId }}>
        {t('features.agentTeams.openListing', 'Open the listing')}
      </Link>
    </Typography>
  );
}

type TeamsListProps = TeamsPanelProps & { readonly projectId: string; readonly abilities: ReturnType<typeof useTeamAbilities> };

/** The header row: the intro on the left, New team and the page's actions on the right. */
function TeamsHeader({ canCreate, onCreate, actions }: { readonly canCreate: boolean; readonly onCreate: () => void; readonly actions: ReactNode }): ReactNode {
  return (
    <Box sx={headerSx}>
      <Typography variant="bodyMedium" color="text.secondary" sx={introSx}>
        {t('features.agentTeams.intro', 'A team is a saved set of agents with its lead. Start a conversation with all of them at once.')}
      </Typography>
      <Box sx={headerActionsSx}>
        {actions}
        {canCreate && (
          <BaseBtn variant="contained" onClick={onCreate} data-testid="agent-teams-create">
            {t('features.agentTeams.create', 'New team')}
          </BaseBtn>
        )}
      </Box>
    </Box>
  );
}

function TeamsList({ projectId, abilities, onStarted, creating, onCreatingChange, headerActions, renderMemberAvatar }: TeamsListProps): ReactNode {
  const list = useAgentTeamList(projectId, '');
  const writes = useAgentTeamWrites(projectId);
  const [editingTeam, setEditingTeam] = useState<AgentTeam | undefined>(undefined);
  const [deleting, setDeleting] = useState<AgentTeam | undefined>(undefined);
  const [deleteError, setDeleteError] = useState<string | undefined>(undefined);
  const [startError, setStartError] = useState<string | undefined>(undefined);
  const teamExport = useTeamExport();
  const teamPublish = useTeamPublish(projectId);

  const start = (team: AgentTeam): void => {
    setStartError(undefined);
    writes.start.mutate(team.id, {
      onSuccess: (conversation) => onStarted(conversation),
      onError: (cause) => setStartError(serverErrorMessage(cause, t('features.agentTeams.startFailed', 'The conversation was not started. Try again.'))),
    });
  };

  const confirmDelete = (): void => {
    if (deleting === undefined) return;
    setDeleteError(undefined);
    writes.remove.mutate(deleting.id, {
      onSuccess: () => setDeleting(undefined),
      onError: (cause) => setDeleteError(serverErrorMessage(cause, t('features.agentTeams.deleteFailed', 'The team was not deleted. Try again.'))),
    });
  };

  const teams = list.rows;
  const avatars = useTeamMemberAvatars(projectId, teams.length > 0);
  const editOpen = creating || editingTeam !== undefined;
  return (
    <Box sx={panelSx} data-testid="agent-teams-panel">
      <TeamsHeader canCreate={abilities.canCreate} onCreate={() => onCreatingChange(true)} actions={headerActions} />
      {[startError, teamExport.error].map((message, index) =>
        message === undefined ? null : (
          <Alert key={ALERT_IDS[index]} testId={ALERT_IDS[index] ?? ''}>
            {message}
          </Alert>
        ),
      )}
      <PublishNotices error={teamPublish.error} published={teamPublish.published} />
      {list.isError && (
        <Alert testId="agent-teams-load-error">{serverErrorMessage(list.error, t('features.agentTeams.loadFailed', 'The teams could not be loaded.'))}</Alert>
      )}
      {list.isSuccess && teams.length === 0 && (
        <TeamsEmpty
          canCreate={abilities.canCreate}
          onCreate={() => onCreatingChange(true)}
        />
      )}
      {teams.length > 0 && (
        <Box sx={gridSx}>
          {teams.map((team) => (
            <TeamCard
              key={team.id}
              team={team}
              avatars={avatars}
              renderAvatar={renderMemberAvatar}
              starting={writes.start.isPending}
              onStart={() => start(team)}
              exporting={teamExport.exportingId === team.id}
              onExport={() => void teamExport.exportTeam(team)}
              onPublish={abilities.canPublish ? () => teamPublish.publish(team) : undefined}
              publishing={teamPublish.publishingId === team.id}
              onEdit={abilities.canUpdate ? () => setEditingTeam(team) : undefined}
              onDelete={abilities.canDelete ? () => { setDeleteError(undefined); setDeleting(team); } : undefined}
            />
          ))}
        </Box>
      )}
      {list.hasMore && (
        <Box sx={moreSx}>
          <Typography variant="bodySmall" color="text.secondary" data-testid="agent-teams-count">
            {t('features.agentTeams.shownOf', 'Showing {{shown}} of {{total}} teams', { shown: teams.length, total: list.total })}
          </Typography>
          <BaseBtn variant="outlined" disabled={list.loadingMore} onClick={list.loadMore} data-testid="agent-teams-more">
            {t('features.agentTeams.loadMore', 'Show more')}
          </BaseBtn>
        </Box>
      )}
      <TeamEditDialog
        projectId={projectId}
        open={editOpen}
        team={creating ? undefined : editingTeam}
        renderAvatar={renderMemberAvatar}
        onClose={() => {
          setEditingTeam(undefined);
          onCreatingChange(false);
        }}
      />
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

/** The list page's own gutter (`EntityCardList`'s container), so the header lines up with the grid below. */
const panelSx: SxProps<Theme> = (theme: Theme) => ({
  boxSizing: 'border-box',
  display: 'flex',
  flexDirection: 'column',
  gap: theme.spacing(2),
  width: '100%',
  minWidth: 0,
  padding: theme.spacing(2.5, 3, 3),
});

const headerSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'space-between',
  flexWrap: 'wrap',
  gap: theme.spacing(2),
});

const introSx: SxProps<Theme> = { flex: '1 1 20rem', minWidth: 0 };

const headerActionsSx: SxProps<Theme> = (theme: Theme) => ({ display: 'flex', alignItems: 'center', gap: theme.spacing(1), flexShrink: 0 });

/** The Agents grid's tracks (`EntityCardList`'s rich variant), so both tabs read as one product. */
const gridSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'grid',
  gridTemplateColumns: 'repeat(auto-fill, minmax(320px, 1fr))',
  gridAutoRows: '1fr',
  gap: theme.spacing(2),
  width: '100%',
  minWidth: 0,
});

const moreSx: SxProps<Theme> = (theme: Theme) => ({ display: 'flex', alignItems: 'center', gap: theme.spacing(2) });
