/** A team's members as an overlapping avatar stack, lead first with its badge, then "+N" (issue 3134). */
import { useState, type ReactNode } from 'react';

import StarRoundedIcon from '@mui/icons-material/StarRounded';
import Box from '@mui/material/Box';
import ButtonBase from '@mui/material/ButtonBase';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import type { AgentTeam, AgentTeamMember } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { Monogram } from '@/shared/ui/Monogram';

import { memberAvatarKey, type MemberAvatar } from '../model/useTeamMemberAvatars';

/** Draws one member's live avatar; the page passes the shared renderer (a feature does not import another feature). */
export type RenderMemberAvatar = (avatar: MemberAvatar, size: string) => ReactNode;

const SIZE = '2rem';
const SHOWN = 5;

interface OrderedMember {
  readonly member: AgentTeamMember;
  readonly lead: boolean;
}

function memberName(member: AgentTeamMember): string {
  return member.name === '' ? String(member.application_id) : member.name;
}

/** The members with the lead first, the rest in their saved order. */
function orderedMembers(team: AgentTeam): OrderedMember[] {
  const all = team.members.map((member, index) => ({ member, lead: team.lead_position === index + 1 }));
  return [...all.filter((entry) => entry.lead), ...all.filter((entry) => !entry.lead)];
}

function MemberFace({ entry, avatar, renderAvatar }: { readonly entry: OrderedMember; readonly avatar: MemberAvatar | undefined; readonly renderAvatar: RenderMemberAvatar | undefined }): ReactNode {
  const face = avatar !== undefined && renderAvatar !== undefined ? renderAvatar(avatar, SIZE) : <Monogram name={memberName(entry.member)} size={SIZE} variant="labelSmall" />;
  return (
    <Box
      data-testid={entry.lead ? 'agent-team-lead-avatar' : 'agent-team-member-avatar'}
      sx={faceSx}
    >
      {face}
      {entry.lead && (
        <Box
          component="span"
          data-testid="agent-team-lead-badge"
          sx={leadBadgeSx}
        >
          <StarRoundedIcon
            fontSize="inherit"
            titleAccess={t('features.agentTeams.leadBadge', 'Lead')}
          />
        </Box>
      )}
    </Box>
  );
}

interface TeamAvatarStackProps {
  readonly team: AgentTeam;
  readonly avatars: ReadonlyMap<string, MemberAvatar>;
  readonly renderAvatar?: RenderMemberAvatar | undefined;
}

/**
 * The stack, its tooltip naming every member, and a disclosure button that
 * lists them (the lead marked) for a keyboard or screen-reader user and on a
 * device that cannot hover.
 */
export function TeamAvatarStack({ team, avatars, renderAvatar }: TeamAvatarStackProps): ReactNode {
  const [expanded, setExpanded] = useState(false);
  const members = orderedMembers(team);
  const shown = members.slice(0, SHOWN);
  const extra = members.length - shown.length;
  const names = members.map((entry) => (entry.lead ? t('features.agentTeams.leadMember', '{{name}} (lead)', { name: memberName(entry.member) }) : memberName(entry.member)));
  const listId = `agent-team-members-${String(team.id)}`;
  return (
    <Box sx={stackColumnSx}>
      <Box sx={stackRowSx}>
        <Tooltip title={names.join(', ')}>
          <Box
            data-testid={`agent-team-avatars-${String(team.id)}`}
            sx={facesSx}
          >
            {shown.map((entry) => (
              <MemberFace
                key={`${String(entry.member.project_id)}:${String(entry.member.application_id)}`}
                entry={entry}
                avatar={avatars.get(memberAvatarKey(entry.member.project_id, entry.member.application_id))}
                renderAvatar={renderAvatar}
              />
            ))}
            {extra > 0 && (
              <Box
                component="span"
                data-testid="agent-team-avatars-more"
                sx={moreSx}
              >
                {`+${String(extra)}`}
              </Box>
            )}
          </Box>
        </Tooltip>
        <ButtonBase
          aria-expanded={expanded}
          aria-controls={listId}
          data-testid={`agent-team-members-toggle-${String(team.id)}`}
          onClick={() => setExpanded((open) => !open)}
          sx={toggleSx}
        >
          {t('features.agentTeams.memberCount', '{{count}} agents', { count: members.length })}
        </ButtonBase>
      </Box>
      {expanded && (
        <Box
          component="ul"
          id={listId}
          aria-label={t('features.agentTeams.membersList', 'Members of {{name}}', { name: team.name })}
          sx={listSx}
        >
          {names.map((name, index) => (
            <Typography
              key={`${String(index)}:${name}`}
              component="li"
              variant="bodySmall"
            >
              {name}
            </Typography>
          ))}
        </Box>
      )}
    </Box>
  );
}

const stackColumnSx: SxProps<Theme> = (theme: Theme) => ({ display: 'flex', flexDirection: 'column', gap: theme.spacing(1), minWidth: 0 });

const stackRowSx: SxProps<Theme> = (theme: Theme) => ({ display: 'flex', alignItems: 'center', gap: theme.spacing(1.5), minWidth: 0 });

const facesSx: SxProps<Theme> = { display: 'flex', alignItems: 'center' };

/** Each face overlaps the one before by a quarter, ringed in the card's surface so the overlap reads. */
const faceSx: SxProps<Theme> = (theme: Theme) => ({
  position: 'relative',
  display: 'flex',
  borderRadius: theme.vars.shape.radiusPill,
  boxShadow: `0 0 0 2px ${theme.vars.palette.background.card.default}`,
  '& + &': { marginLeft: theme.spacing(-1) },
});

const leadBadgeSx: SxProps<Theme> = (theme: Theme) => ({
  position: 'absolute',
  right: theme.spacing(-0.5),
  bottom: theme.spacing(-0.5),
  display: 'inline-flex',
  borderRadius: theme.vars.shape.radiusPill,
  fontSize: theme.typography.labelSmall.fontSize,
  padding: theme.spacing(0.125),
  background: theme.vars.palette.primary.main,
  color: theme.vars.palette.primary.contrastText,
  boxShadow: `0 0 0 2px ${theme.vars.palette.background.card.default}`,
});

const moreSx: SxProps<Theme> = (theme: Theme) => ({
  ...theme.typography.labelSmall,
  marginLeft: theme.spacing(-1),
  width: SIZE,
  height: SIZE,
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'center',
  borderRadius: theme.vars.shape.radiusPill,
  background: theme.vars.palette.background.card.default,
  border: `1px solid ${theme.vars.palette.border.lines}`,
  boxSizing: 'border-box',
  color: theme.vars.palette.text.primary,
  boxShadow: `0 0 0 2px ${theme.vars.palette.background.card.default}`,
});

const toggleSx: SxProps<Theme> = (theme: Theme) => ({
  ...theme.typography.bodySmall,
  color: theme.vars.palette.text.primary,
  borderRadius: theme.vars.shape.radiusSm,
  textDecoration: 'underline',
  textUnderlineOffset: '0.2em',
});

const listSx: SxProps<Theme> = (theme: Theme) => ({
  margin: 0,
  paddingLeft: theme.spacing(2.5),
  color: theme.vars.palette.text.primary,
  display: 'flex',
  flexDirection: 'column',
  gap: theme.spacing(0.25),
});
