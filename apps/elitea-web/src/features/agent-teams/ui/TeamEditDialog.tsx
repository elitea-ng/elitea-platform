/**
 * Create or edit a saved team (issue 1620): its name, description, agents
 * (in the order they join a conversation) and lead.
 *
 * Agents are picked from the project's own agents. A new member runs the
 * agent's default version; a member saved from a conversation keeps the
 * version it ran there. A refusal stays in the dialog, in the server's words
 * (a name already taken, an agent that is gone).
 */
import type { ChangeEvent, ReactNode } from 'react';
import { useState } from 'react';

import Box from '@mui/material/Box';
import Chip from '@mui/material/Chip';
import IconButton from '@mui/material/IconButton';
import List from '@mui/material/List';
import ListItem from '@mui/material/ListItem';
import ListItemText from '@mui/material/ListItemText';
import TextField from '@mui/material/TextField';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';
import CloseIcon from '@mui/icons-material/Close';
import StarIcon from '@mui/icons-material/Star';
import StarBorderIcon from '@mui/icons-material/StarBorder';

import type { AgentTeam, Application } from '@/shared/api/generated/model';
import { serverErrorMessage } from '@/shared/api/serverErrorMessage';
import { t } from '@/shared/i18n';
import { BaseModal } from '@/shared/ui/BaseModal';

import { fetchAgentVersions } from '../api/agentTeamsApi';
import {
  MAX_TEAM_MEMBERS,
  defaultVersionOf,
  draftOf,
  draftProblem,
  withMember,
  withoutMember,
  writeOf,
  type TeamDraft,
  type TeamDraftProblem,
} from '../lib/teamDraft';
import { useAgentTeamWrites } from '../model/useAgentTeams';
import { TeamAgentPicker } from './TeamAgentPicker';

export interface TeamEditDialogProps {
  readonly projectId: string;
  readonly open: boolean;
  /** The team being edited; absent creates one. */
  readonly team?: AgentTeam | undefined;
  readonly onClose: () => void;
  readonly onSaved?: ((team: AgentTeam) => void) | undefined;
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

function TeamEditForm({ projectId, team, onClose, onSaved }: Omit<TeamEditDialogProps, 'open'>): ReactNode {
  const { create, update } = useAgentTeamWrites(projectId);
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
      actions={{ confirmText: t('features.agentTeams.edit.save', 'Save'), confirming: saving || problem !== undefined }}
      content={
        <Box sx={{ display: 'flex', flexDirection: 'column', gap: '1rem', minWidth: '30rem' }}>
          <TextField
            label={t('features.agentTeams.edit.name', 'Name')}
            value={draft.name}
            onChange={(event: ChangeEvent<HTMLInputElement>) => setDraft({ ...draft, name: event.target.value })}
            slotProps={{ htmlInput: { 'data-testid': 'agent-team-edit-name', maxLength: 128 } }}
          />
          <TextField
            label={t('features.agentTeams.edit.description', 'Description')}
            value={draft.description}
            onChange={(event: ChangeEvent<HTMLInputElement>) => setDraft({ ...draft, description: event.target.value })}
            multiline
            minRows={2}
            slotProps={{ htmlInput: { 'data-testid': 'agent-team-edit-description' } }}
          />
          <MemberList draft={draft} onChange={setDraft} />
          {draft.members.length < MAX_TEAM_MEMBERS && (
            <TeamAgentPicker projectId={projectId} draft={draft} disabled={adding} onPick={addAgent} />
          )}
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

function MemberList({ draft, onChange }: { readonly draft: TeamDraft; readonly onChange: (draft: TeamDraft) => void }): ReactNode {
  if (draft.members.length === 0) {
    return (
      <Typography variant="bodySmall" color="text.secondary" data-testid="agent-team-edit-empty">
        {t('features.agentTeams.edit.noMembers', 'No agents yet. Add the agents this team starts with.')}
      </Typography>
    );
  }
  return (
    <List dense disablePadding data-testid="agent-team-edit-members">
      {draft.members.map((member, index) => {
        const position = index + 1;
        const isLead = draft.lead === position;
        return (
          <ListItem
            key={`${member.project_id}:${member.application_id}`}
            disableGutters
            data-testid={`agent-team-edit-member-${position}`}
            secondaryAction={
              <Box sx={{ display: 'flex', alignItems: 'center', gap: 0.25 }}>
                <Tooltip title={isLead ? t('features.agentTeams.edit.clearLead', 'Clear lead') : t('features.agentTeams.edit.makeLead', 'Make lead')}>
                  <IconButton
                    size="small"
                    aria-label={isLead ? t('features.agentTeams.edit.clearLead', 'Clear lead') : t('features.agentTeams.edit.makeLead', 'Make lead')}
                    aria-pressed={isLead}
                    data-testid={`agent-team-edit-lead-${position}`}
                    onClick={() => onChange({ ...draft, lead: isLead ? 0 : position })}
                  >
                    {isLead ? <StarIcon fontSize="small" /> : <StarBorderIcon fontSize="small" />}
                  </IconButton>
                </Tooltip>
                <IconButton
                  size="small"
                  aria-label={t('features.agentTeams.edit.remove', 'Remove from the team')}
                  data-testid={`agent-team-edit-remove-${position}`}
                  onClick={() => onChange(withoutMember(draft, index))}
                >
                  <CloseIcon fontSize="small" />
                </IconButton>
              </Box>
            }
          >
            <ListItemText
              primary={
                <Box component="span" sx={{ display: 'inline-flex', alignItems: 'center', gap: 0.75 }}>
                  {member.name === '' ? t('features.agentTeams.unnamedAgent', 'Agent {{id}}', { id: member.application_id }) : member.name}
                  {isLead && <Chip size="small" label={t('chat.lead.badge', 'Lead')} />}
                </Box>
              }
            />
          </ListItem>
        );
      })}
    </List>
  );
}
