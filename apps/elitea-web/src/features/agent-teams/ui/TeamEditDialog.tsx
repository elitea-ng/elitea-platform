/**
 * Create or edit a saved team (issue 1620): its name, description, agents
 * (in the order they join a conversation) and lead.
 *
 * Agents are picked from the project's own agents. A new member runs the
 * agent's default version; a member saved from a conversation keeps the
 * version it ran there. A refusal stays in the dialog, in the server's words
 * (a name already taken, an agent that is gone).
 *
 * The layout (issue 3142) is the Import team preview's: the members as a
 * roster with their avatars and the Lead badge, each row's actions named for
 * a screen reader and explained by a tooltip, on the cards' surface tokens.
 */
import type { ChangeEvent, ReactNode } from 'react';
import { useState } from 'react';

import Box from '@mui/material/Box';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import type { AgentTeam, Application } from '@/shared/api/generated/model';
import { serverErrorMessage } from '@/shared/api/serverErrorMessage';
import { t } from '@/shared/i18n';
import { BaseModal } from '@/shared/ui/BaseModal';

import { fetchAgentVersions } from '../api/agentTeamsApi';
import { MAX_TEAM_MEMBERS, defaultVersionOf, draftOf, draftProblem, withMember, writeOf, type TeamDraft, type TeamDraftProblem } from '../lib/teamDraft';
import { useAgentTeamWrites } from '../model/useAgentTeams';
import { useTeamMemberAvatars } from '../model/useTeamMemberAvatars';
import type { RenderMemberAvatar } from './TeamAvatarStack';
import { TeamAgentPicker } from './TeamAgentPicker';
import { TeamMemberList } from './TeamMemberList';

export interface TeamEditDialogProps {
  readonly projectId: string;
  readonly open: boolean;
  /** The team being edited; absent creates one. */
  readonly team?: AgentTeam | undefined;
  readonly onClose: () => void;
  readonly onSaved?: ((team: AgentTeam) => void) | undefined;
  /** Draws each agent's live avatar (the page's shared renderer); absent draws monograms. */
  readonly renderAvatar?: RenderMemberAvatar | undefined;
}

export function TeamEditDialog({ open, team, ...rest }: TeamEditDialogProps): ReactNode {
  if (!open) return null;
  return <TeamEditForm key={team?.id ?? 'new'} team={team} {...rest} />;
}

function problemText(problem: TeamDraftProblem): string {
  switch (problem) {
    case 'name':
      return t('features.agentTeams.edit.nameRequired', 'Name the team.');
    case 'nameTooLong':
      return t('features.agentTeams.edit.nameTooLong', 'A team name is at most 128 characters.');
    case 'descriptionTooLong':
      return t('features.agentTeams.edit.descriptionTooLong', 'A description is at most 2,000 characters.');
    case 'members':
      return t('features.agentTeams.edit.membersRequired', 'Add at least one agent.');
    case 'tooManyMembers':
      return t('features.agentTeams.edit.tooManyMembers', 'A team has at most 16 agents.');
  }
}

function TeamEditForm({ projectId, team, onClose, onSaved, renderAvatar }: Omit<TeamEditDialogProps, 'open'>): ReactNode {
  const { create, update } = useAgentTeamWrites(projectId);
  const avatars = useTeamMemberAvatars(projectId, true);
  const [draft, setDraft] = useState<TeamDraft>(() => draftOf(team));
  const [error, setError] = useState<string | undefined>(undefined);
  const [adding, setAdding] = useState(false);
  const problem = draftProblem(draft);
  const saving = create.isPending || update.isPending;

  const save = (): void => {
    if (problem !== undefined || saving) return;
    setError(undefined);
    const body = writeOf(draft);
    const handlers = {
      onSuccess: (saved: AgentTeam) => {
        onSaved?.(saved);
        onClose();
      },
      onError: (cause: unknown) => setError(serverErrorMessage(cause, t('features.agentTeams.edit.failed', 'The team was not saved. Try again.'))),
    };
    if (team === undefined) create.mutate(body, handlers);
    else update.mutate({ teamId: team.id, team: body }, handlers);
  };

  const addAgent = (agent: Application): void => {
    const applicationId = Number(agent.id);
    const memberProject = Number(agent.project_id ?? projectId);
    setAdding(true);
    setError(undefined);
    fetchAgentVersions(projectId, applicationId)
      .then((detail) => {
        const versionId = defaultVersionOf(detail.versions);
        if (versionId === undefined) {
          setError(t('features.agentTeams.edit.noVersion', 'This agent has no saved version to add.'));
          return;
        }
        setDraft((current) => withMember(current, { application_id: applicationId, project_id: memberProject, version_id: versionId, name: agent.name }));
      })
      .catch((cause: unknown) => setError(serverErrorMessage(cause, t('features.agentTeams.edit.addFailed', 'The agent could not be added. Try again.'))))
      .finally(() => setAdding(false));
  };

  return (
    <BaseModal
      open
      data-testid="agent-team-edit-dialog"
      title={team === undefined ? t('features.agentTeams.edit.createTitle', 'New team') : t('features.agentTeams.edit.editTitle', 'Edit team')}
      onClose={onClose}
      onConfirm={save}
      actions={{ confirmText: t('features.agentTeams.edit.save', 'Save'), confirming: saving, confirmDisabled: problem !== undefined }}
      content={
        <Box sx={formSx}>
          <TextField
            label={t('features.agentTeams.edit.name', 'Name')}
            value={draft.name}
            fullWidth
            onChange={(event: ChangeEvent<HTMLInputElement>) => setDraft({ ...draft, name: event.target.value })}
            slotProps={{ htmlInput: { 'data-testid': 'agent-team-edit-name', maxLength: 128 } }}
          />
          <TextField
            label={t('features.agentTeams.edit.description', 'Description')}
            value={draft.description}
            fullWidth
            onChange={(event: ChangeEvent<HTMLInputElement>) => setDraft({ ...draft, description: event.target.value })}
            multiline
            minRows={2}
            maxRows={6}
            slotProps={{ htmlInput: { 'data-testid': 'agent-team-edit-description' } }}
          />
          <Box component="section" aria-labelledby="agent-team-edit-members-title" sx={sectionSx}>
            <Box sx={sectionHeaderSx}>
              <Typography id="agent-team-edit-members-title" component="h3" variant="labelMedium" sx={sectionTitleSx}>
                {t('features.agentTeams.edit.membersTitle', 'Agents')}
              </Typography>
              <Typography variant="bodySmall" sx={countSx} data-testid="agent-team-edit-count">
                {t('features.agentTeams.edit.memberCount', '{{count}} of {{max}}', { count: draft.members.length, max: MAX_TEAM_MEMBERS })}
              </Typography>
            </Box>
            <Typography variant="bodySmall" sx={countSx}>
              {t('features.agentTeams.edit.orderHint', 'Agents join a conversation in this order.')}
            </Typography>
            <TeamMemberList draft={draft} avatars={avatars} renderAvatar={renderAvatar} onChange={setDraft} />
            {draft.members.length < MAX_TEAM_MEMBERS && (
              <TeamAgentPicker projectId={projectId} draft={draft} disabled={adding} renderAvatar={renderAvatar} onPick={addAgent} />
            )}
          </Box>
          {problem !== undefined && draft.name !== '' && (
            <Typography variant="bodySmall" color="text.secondary" data-testid="agent-team-edit-problem">
              {problemText(problem)}
            </Typography>
          )}
          {error !== undefined && (
            <Typography role="alert" variant="bodySmall" color="error" data-testid="agent-team-edit-error">
              {error}
            </Typography>
          )}
        </Box>
      }
    />
  );
}

const formSx: SxProps<Theme> = (theme: Theme) => ({ display: 'flex', flexDirection: 'column', gap: theme.spacing(2), minWidth: 0, paddingTop: theme.spacing(1) });
const sectionSx: SxProps<Theme> = (theme: Theme) => ({ display: 'flex', flexDirection: 'column', gap: theme.spacing(1), minWidth: 0 });
const sectionHeaderSx: SxProps<Theme> = (theme: Theme) => ({ display: 'flex', alignItems: 'baseline', justifyContent: 'space-between', gap: theme.spacing(1) });
const sectionTitleSx: SxProps<Theme> = (theme: Theme) => ({ margin: 0, color: theme.vars.palette.text.primary });
const countSx: SxProps<Theme> = (theme: Theme) => ({ color: theme.vars.palette.text.secondary });
