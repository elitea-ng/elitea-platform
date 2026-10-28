/**
 * ResourceCard — one neutral card on the Help Center.
 *
 * Ported from `apps/elitea-ui/src/[fsd]/pages/resources/ui/ResourceCard.jsx`.
 *
 * Brand guide v3: every card looks the same. The card sits on the `elevated`
 * surface with a 1px hairline border and the large radius. There is no
 * gradient wash and no per-card colour. The icon well is neutral too. The
 * old per-scheme `background.resourceCard.<scheme>.*` tokens are not read.
 *
 * The card itself is not interactive (it has no click target). The hover
 * state only makes the border stronger; the links inside carry the focus ring.
 *
 * NOTE: sx values are defined as plain objects to avoid TS2769 "No overload
 * matches" errors from MUI's strict sx prop typing.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Divider from '@mui/material/Divider';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import type { ResourceCardProps } from '../lib/ResourceCard.types';

/** A neutral card with an icon, a title, a description and children (links or the empty state). */
export function ResourceCard({ title, description, icon, tourTargetId, children }: ResourceCardProps): ReactNode {
  return (
    <Box
      sx={cardSx}
      data-tour={tourTargetId}
    >
      <Box sx={cardHeaderSx}>
        <Box
          sx={iconWellSx}
          aria-hidden
        >
          {icon}
        </Box>
        <Box sx={headerTextSx}>
          <Typography
            variant="headingMedium"
            component="h2"
            sx={titleSx}
          >
            {title}
          </Typography>
          <Typography
            variant="bodySmall"
            sx={descriptionSx}
          >
            {description}
          </Typography>
        </Box>
      </Box>
      <Divider sx={dividerSx} />
      <Box sx={bodySx}>{children}</Box>
    </Box>
  );
}

ResourceCard.displayName = 'ResourceCard';

/**
 * Card container. Exported so that the tests can resolve each token against
 * the real theme. It is deliberately NOT annotated `: SxProps<Theme>`: the
 * tests need the concrete object type, not the widened union.
 */
export const cardSx = {
  display: 'flex',
  flexDirection: 'column',
  flexShrink: 0,
  minWidth: 0,
  width: '100%',
  maxWidth: '31.25rem',
  minHeight: '14.25rem',
  backgroundColor: (t: Theme) => t.vars.palette.background.modal.simple,
  border: '0.0625rem solid',
  borderColor: (t: Theme) => t.vars.palette.border.cardsOutlines,
  borderRadius: (t: Theme) => t.vars.shape.radiusLg,
  transition: 'border-color 120ms ease',
  '&:hover': {
    borderColor: (t: Theme) => t.vars.palette.border.hover,
  },
} as const;

/** Neutral icon well: surface fill, hairline border, icon in the muted text colour. */
export const iconWellSx = {
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'center',
  flexShrink: 0,
  width: '2.5rem',
  height: '2.5rem',
  backgroundColor: (t: Theme) => t.vars.palette.background.card.default,
  border: '0.0625rem solid',
  borderColor: (t: Theme) => t.vars.palette.border.cardsOutlines,
  borderRadius: (t: Theme) => t.vars.shape.radiusMd,
  color: (t: Theme) => t.vars.palette.icon.fill.default,
} as const;

/**
 * Card title: sentence case, weight 700, NOT tracked uppercase. The heading
 * variant carries the display face of the brand pack.
 */
export const titleSx = {
  color: (t: Theme) => t.vars.palette.text.secondary,
  fontWeight: 700,
  textTransform: 'none',
  letterSpacing: 'normal',
} as const;

const descriptionSx = {
  color: (t: Theme) => t.vars.palette.text.primary,
} as const;

const cardHeaderSx = {
  display: 'flex',
  flexDirection: 'row',
  alignItems: 'center',
  gap: '1rem',
  px: '1rem',
  py: '0.75rem',
} as const;

const headerTextSx = {
  display: 'flex',
  flexDirection: 'column',
  gap: '0.25rem',
  flex: 1,
  minWidth: 0,
} as const;

const dividerSx = {
  borderColor: (t: Theme) => t.vars.palette.border.cardsOutlines,
} as const;

const bodySx = {
  display: 'flex',
  flexDirection: 'column',
  gap: (t: Theme) => t.spacing(1),
  px: '1.5rem',
  py: '0.75rem',
  flex: 1,
} as const;
