/**
 * The Edit team dialog's members (issue 3142): one roster row per agent with
 * its avatar, name and Lead badge, and four actions — move up, move down
 * (the join order), make or clear the lead, remove. Each action has a
 * tooltip and an accessible name that says which agent it acts on, and the
 * lead toggle says its state (aria-pressed).
 */
import type { ReactNode } from 'react';

import ArrowDownwardIcon from '@mui/icons-material/ArrowDownward';
import ArrowUpwardIcon from '@mui/icons-material/ArrowUpward';
import CloseIcon from '@mui/icons-material/Close';
import StarIcon from '@mui/icons-material/Star';
import StarBorderIcon from '@mui/icons-material/StarBorder';
import IconButton from '@mui/material/IconButton';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import type { AgentTeamMember } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { AgentRoster, AgentRosterRow, ROSTER_AVATAR_SIZE } from '@/shared/ui/AgentRoster';
import { Monogram } from '@/shared/ui/Monogram';

import { withMemberMoved, withoutMember, type TeamDraft } from '../lib/teamDraft';
import { memberAvatarKey, type MemberAvatar } from '../model/useTeamMemberAvatars';
import type { RenderMemberAvatar } from './TeamAvatarStack';

export interface TeamMemberListProps {
  readonly draft: TeamDraft;
  readonly avatars: ReadonlyMap<string, MemberAvatar>;
  readonly renderAvatar: RenderMemberAvatar | undefined;
  readonly onChange: (draft: TeamDraft) => void;
}

function memberName(member: AgentTeamMember): string {
  return member.name === '' ? t('features.agentTeams.unnamedAgent', 'Agent {{id}}', { id: member.application_id }) : member.name;
}

/** One icon action with its tooltip; the tooltip text is its accessible name too. */
function RowAction({ label, disabled = false, pressed, testId, onClick, children }: {
  readonly label: string;
  readonly disabled?: boolean;
  readonly pressed?: boolean | undefined;
  readonly testId: string;
  readonly onClick: () => void;
  readonly children: ReactNode;
}): ReactNode {
  const button = (
    <IconButton size="small" aria-label={label} aria-pressed={pressed} disabled={disabled} data-testid={testId} onClick={onClick}>
      {children}
    </IconButton>
  );
  // A disabled button fires no pointer events; the span keeps its tooltip.
  return <Tooltip title={label}>{disabled ? <span>{button}</span> : button}</Tooltip>;
}

function MemberActions({ draft, index, name, onChange }: { readonly draft: TeamDraft; readonly index: number; readonly name: string; readonly onChange: (draft: TeamDraft) => void }): ReactNode {
  const position = index + 1;
  const isLead = draft.lead === position;
  return (
    <>
      <RowAction
        label={t('features.agentTeams.edit.moveUp', 'Move {{name}} up', { name })}
        disabled={index === 0}
        testId={`agent-team-edit-up-${position}`}
        onClick={() => onChange(withMemberMoved(draft, index, -1))}
      >
        <ArrowUpwardIcon fontSize="small" />
      </RowAction>
      <RowAction
        label={t('features.agentTeams.edit.moveDown', 'Move {{name}} down', { name })}
        disabled={index === draft.members.length - 1}
        testId={`agent-team-edit-down-${position}`}
        onClick={() => onChange(withMemberMoved(draft, index, 1))}
      >
        <ArrowDownwardIcon fontSize="small" />
      </RowAction>
      <RowAction
        label={isLead ? t('features.agentTeams.edit.clearLeadOf', 'Clear {{name}} as the lead', { name }) : t('features.agentTeams.edit.makeLeadOf', 'Make {{name}} the lead', { name })}
        pressed={isLead}
        testId={`agent-team-edit-lead-${position}`}
        onClick={() => onChange({ ...draft, lead: isLead ? 0 : position })}
      >
        {isLead ? <StarIcon fontSize="small" color="primary" /> : <StarBorderIcon fontSize="small" />}
      </RowAction>
      <RowAction
        label={t('features.agentTeams.edit.removeAgent', 'Remove {{name}} from the team', { name })}
        testId={`agent-team-edit-remove-${position}`}
        onClick={() => onChange(withoutMember(draft, index))}
      >
        <CloseIcon fontSize="small" />
      </RowAction>
    </>
  );
}

export function TeamMemberList({ draft, avatars, renderAvatar, onChange }: TeamMemberListProps): ReactNode {
  if (draft.members.length === 0) {
    return (
      <Typography variant="bodySmall" color="text.secondary" data-testid="agent-team-edit-empty">
        {t('features.agentTeams.edit.noMembers', 'No agents yet. Add the agents this team starts with.')}
      </Typography>
    );
  }
  return (
    <AgentRoster label={t('features.agentTeams.edit.membersLabel', 'Agents of this team')} data-testid="agent-team-edit-members">
      {draft.members.map((member, index) => {
        const name = memberName(member);
        const avatar = avatars.get(memberAvatarKey(member.project_id, member.application_id));
        return (
          <AgentRosterRow
            key={`${member.project_id}:${member.application_id}`}
            data-testid={`agent-team-edit-member-${index + 1}`}
            face={avatar !== undefined && renderAvatar !== undefined ? renderAvatar(avatar, ROSTER_AVATAR_SIZE) : <Monogram name={name} size={ROSTER_AVATAR_SIZE} variant="labelSmall" />}
            name={name}
            lead={draft.lead === index + 1}
            actions={<MemberActions draft={draft} index={index} name={name} onChange={onChange} />}
          />
        );
      })}
    </AgentRoster>
  );
}
