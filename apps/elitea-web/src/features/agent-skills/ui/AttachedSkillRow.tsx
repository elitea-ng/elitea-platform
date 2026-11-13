import type { ReactNode } from 'react';

import DeleteIcon from '@mui/icons-material/Delete';
import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { OpenNewIcon } from '@/shared/ui/icons/open-new-icon';

import type { AgentSkill } from '../api/agentSkillsApi';

/**
 * One attached skill, drawn like an attached toolkit (`features/agents`'
 * `ToolCard`): a bordered card, the name over the description, and an
 * open-in-new-tab and a delete icon on the right. It was a bare text row with
 * only a delete icon (issue 2052).
 */
export interface AttachedSkillRowProps {
  readonly skill: AgentSkill;
  /** Absent in a read-only view: the card stays, its controls go. */
  readonly onOpen: (() => void) | undefined;
  readonly onDetach: (() => void) | undefined;
  readonly isDetaching: boolean;
}

export function AttachedSkillRow({ skill, onOpen, onDetach, isDetaching }: AttachedSkillRowProps): ReactNode {
  return (
    <Box
      sx={cardSx}
      data-testid="agent-attached-skill"
    >
      <Box sx={textSx}>
        <Typography
          variant="bodyMedium"
          sx={clampSx}
        >
          {skill.name}
        </Typography>
        {skill.description !== undefined && skill.description !== '' && (
          <Typography
            variant="bodySmall"
            color="text.secondary"
            sx={clampSx}
          >
            {skill.description}
          </Typography>
        )}
      </Box>
      {onOpen !== undefined && (
        <Tooltip
          title={t('features.agentSkills.open', 'Open skill in a new tab')}
          placement="top"
        >
          <IconButton
            color="tertiary"
            size="small"
            aria-label={t('features.agentSkills.openAria', 'open in new tab')}
            data-testid={`agent-open-skill-${String(skill.id)}`}
            onClick={onOpen}
          >
            <Box
              component={OpenNewIcon}
              sx={iconSx}
            />
          </IconButton>
        </Tooltip>
      )}
      {onDetach !== undefined && (
        <Tooltip
          title={t('features.agentSkills.detach', 'Detach skill')}
          placement="top"
        >
          <IconButton
            color="tertiary"
            size="small"
            aria-label={t('features.agentSkills.detach', 'Detach skill')}
            data-testid={`agent-detach-skill-${String(skill.id)}`}
            disabled={isDetaching}
            onClick={onDetach}
          >
            <DeleteIcon sx={iconSx} />
          </IconButton>
        </Tooltip>
      )}
    </Box>
  );
}

const cardSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  alignItems: 'center',
  gap: '0.25rem',
  padding: '0.5rem 0.75rem',
  boxSizing: 'border-box',
  borderRadius: theme.vars.shape.radiusMd,
  backgroundColor: theme.vars.palette.background.userInputBackground,
  border: `0.0625rem solid ${theme.vars.palette.border.table}`,
});
const textSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', flex: 1, minWidth: 0 };
const clampSx: SxProps<Theme> = { overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' };
const iconSx: SxProps<Theme> = { width: '1rem', height: '1rem' };
