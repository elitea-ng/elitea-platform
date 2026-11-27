/**
 * A team's agents as a list (issue 3142): one row per agent with its avatar,
 * name, an optional second line and a Lead badge, and the row's own actions
 * at the end. The Import team preview and the Edit team dialog draw the same
 * rows, on the Agents and Teams cards' surface tokens, so the two dialogs
 * read as one product.
 *
 * The avatar is passed in (`face`): the live avatar renderer lives in a
 * feature, which `shared/` may not import. Every colour is a theme token, so
 * a brand pack and both schemes re-theme it.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';

export interface AgentRosterProps {
  /** The list's accessible name, e.g. "Members of Dev team". */
  readonly label: string;
  readonly children: ReactNode;
  readonly 'data-testid'?: string | undefined;
}

/** The list surface: a hairline box with the rows divided by the same hairline. */
export function AgentRoster({ label, children, 'data-testid': testId }: AgentRosterProps): ReactNode {
  return (
    <Box
      component="ul"
      aria-label={label}
      data-testid={testId}
      sx={rosterSx}
    >
      {children}
    </Box>
  );
}

export interface AgentRosterRowProps {
  /** The agent's avatar, already sized (`ROSTER_AVATAR_SIZE`). */
  readonly face: ReactNode;
  readonly name: string;
  /** A muted second line: a description or the model. */
  readonly detail?: string | undefined;
  readonly lead?: boolean | undefined;
  /** Buttons at the end of the row; each names itself (aria-label). */
  readonly actions?: ReactNode;
  readonly 'data-testid'?: string | undefined;
}

/** The edge of a roster row's avatar. */
export const ROSTER_AVATAR_SIZE = '2rem';

/** The Lead badge of a roster row and of the Lead line of a summary. */
export function LeadBadge(): ReactNode {
  return (
    <Box
      component="span"
      data-testid="agent-roster-lead-badge"
      sx={leadBadgeSx}
    >
      {t('chat.lead.badge', 'Lead')}
    </Box>
  );
}

export function AgentRosterRow({ face, name, detail, lead = false, actions, 'data-testid': testId }: AgentRosterRowProps): ReactNode {
  return (
    <Box
      component="li"
      data-testid={testId}
      sx={rowSx}
    >
      <Box
        sx={faceSx}
        aria-hidden
      >
        {face}
      </Box>
      <Box sx={textSx}>
        <Box sx={nameLineSx}>
          <Typography
            component="span"
            variant="labelMedium"
            sx={nameSx}
          >
            {name}
          </Typography>
          {lead && <LeadBadge />}
        </Box>
        {detail !== undefined && detail !== '' && (
          <Typography
            component="span"
            variant="bodySmall"
            sx={detailSx}
          >
            {detail}
          </Typography>
        )}
      </Box>
      {actions !== undefined && <Box sx={actionsSx}>{actions}</Box>}
    </Box>
  );
}

const rosterSx: SxProps<Theme> = (theme: Theme) => ({
  listStyle: 'none',
  margin: 0,
  padding: 0,
  borderRadius: theme.vars.shape.radiusMd,
  border: `1px solid ${theme.vars.palette.border.lines}`,
  background: theme.vars.palette.background.card.default,
  overflow: 'hidden',
});

const rowSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  alignItems: 'center',
  gap: theme.spacing(1.5),
  minHeight: theme.spacing(6),
  padding: theme.spacing(1, 1.5),
  boxSizing: 'border-box',
  '& + &': { borderTop: `1px solid ${theme.vars.palette.border.lines}` },
});

const faceSx: SxProps<Theme> = { display: 'flex', flexShrink: 0 };

const textSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', flex: 1, minWidth: 0 };

const nameLineSx: SxProps<Theme> = (theme: Theme) => ({ display: 'flex', alignItems: 'center', gap: theme.spacing(1), minWidth: 0 });

const nameSx: SxProps<Theme> = (theme: Theme) => ({
  color: theme.vars.palette.text.primary,
  overflow: 'hidden',
  textOverflow: 'ellipsis',
  whiteSpace: 'nowrap',
  minWidth: 0,
});

const detailSx: SxProps<Theme> = (theme: Theme) => ({
  color: theme.vars.palette.text.secondary,
  overflow: 'hidden',
  textOverflow: 'ellipsis',
  whiteSpace: 'nowrap',
});

const actionsSx: SxProps<Theme> = (theme: Theme) => ({ display: 'flex', alignItems: 'center', gap: theme.spacing(0.5), flexShrink: 0 });

const leadBadgeSx: SxProps<Theme> = (theme: Theme) => ({
  ...theme.typography.labelSmall,
  flexShrink: 0,
  padding: theme.spacing(0.25, 1),
  borderRadius: theme.vars.shape.radiusPill,
  background: theme.vars.palette.primary.main,
  color: theme.vars.palette.primary.contrastText,
});
