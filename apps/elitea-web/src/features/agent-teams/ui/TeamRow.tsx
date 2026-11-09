/** One saved team in the Teams tab: its name, agents (the lead marked) and actions. */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Chip from '@mui/material/Chip';
import Paper from '@mui/material/Paper';
import Typography from '@mui/material/Typography';

import type { AgentTeam, AgentTeamMember } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

export interface TeamRowProps {
  readonly team: AgentTeam;
  readonly starting: boolean;
  readonly onStart: () => void;
  /** Absent for a caller who may not edit teams. */
  readonly onEdit?: (() => void) | undefined;
  /** Absent for a caller who may not delete teams. */
  readonly onDelete?: (() => void) | undefined;
}

function memberLabel(member: AgentTeamMember, isLead: boolean): string {
  const name = member.name === '' ? String(member.application_id) : member.name;
  return isLead ? t('features.agentTeams.leadMember', '{{name}} (lead)', { name }) : name;
}

export function TeamRow({ team, starting, onStart, onEdit, onDelete }: TeamRowProps): ReactNode {
  return (
    <Paper variant="outlined" sx={{ p: '0.75rem 1rem', display: 'flex', alignItems: 'center', gap: '1rem' }} data-testid={`agent-team-${team.id}`}>
      <Box sx={{ flex: 1, minWidth: 0, display: 'flex', flexDirection: 'column', gap: 0.5 }}>
        <Typography variant="labelMedium" data-testid={`agent-team-name-${team.id}`}>{team.name}</Typography>
        {team.description !== '' && (
          <Typography variant="bodySmall" color="text.secondary">{team.description}</Typography>
        )}
        <Box sx={{ display: 'flex', flexWrap: 'wrap', gap: 0.5 }}>
          {team.members.map((member, index) => {
            const isLead = team.lead_position === index + 1;
            return (
              <Chip
                key={`${member.project_id}:${member.application_id}`}
                size="small"
                variant={isLead ? 'filled' : 'outlined'}
                label={memberLabel(member, isLead)}
              />
            );
          })}
        </Box>
      </Box>
      <Box sx={{ display: 'flex', gap: 0.5, flexShrink: 0 }}>
        <Button size="small" variant="contained" disabled={starting} onClick={onStart} data-testid={`agent-team-start-${team.id}`}>
          {t('features.agentTeams.start', 'Start conversation')}
        </Button>
        {onEdit !== undefined && (
          <Button size="small" onClick={onEdit} data-testid={`agent-team-edit-${team.id}`}>
            {t('features.agentTeams.editAction', 'Edit')}
          </Button>
        )}
        {onDelete !== undefined && (
          <Button size="small" color="error" onClick={onDelete} data-testid={`agent-team-delete-${team.id}`}>
            {t('features.agentTeams.deleteAction', 'Delete')}
          </Button>
        )}
      </Box>
    </Paper>
  );
}
