import type { SxProps, Theme } from '@mui/material/styles';

import { cardSurfaceSx } from '@/shared/lib/cardSurface';

/**
 * The rich card's styles (`EntityRichCard.tsx`, issue 3133). The surface,
 * hover lift and focus ring are `cardSurfaceSx`'s, shared with the Teams
 * cards; every colour here is a theme token too.
 */

/** The avatar edge: inside the 48-56px the issue asks for. */
const AVATAR_SIZE = '3.25rem';

const ACTIONS = '[data-entity-card-actions]';
const REDUCED_MOTION = '@media (prefers-reduced-motion: reduce)';
const NO_HOVER = '@media (hover: none)';

export function cardSx(clickable: boolean, selected: boolean): SxProps<Theme> {
  return (theme: Theme) => ({
    ...cardSurfaceSx(theme, '[data-entity-card-open]'),
    width: '100%',
    height: '100%',
    minHeight: '11rem',
    display: 'flex',
    flexDirection: 'column',
    gap: theme.spacing(1.5),
    padding: theme.spacing(2),
    cursor: clickable ? 'pointer' : 'default',
    ...(selected ? { outline: `0.125rem solid ${theme.vars.palette.border.flowNode}`, outlineOffset: '-0.125rem' } : {}),
    '&:hover [data-entity-select]': { opacity: 1 },
    [`& ${ACTIONS}`]: { opacity: 0 },
    [`&:hover ${ACTIONS}, &:focus-within ${ACTIONS}`]: { opacity: 1 },
    [NO_HOVER]: { [`& ${ACTIONS}`]: { opacity: 1, position: 'static', alignSelf: 'flex-end' } },
  });
}

export const headerSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  alignItems: 'center',
  gap: theme.spacing(1.5),
  minWidth: 0,
});

export const avatarSx: SxProps<Theme> = (theme: Theme) => ({
  flexShrink: 0,
  width: AVATAR_SIZE,
  height: AVATAR_SIZE,
  borderRadius: theme.vars.shape.radiusPill,
  overflow: 'hidden',
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'center',
});

export const headingSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  flexDirection: 'column',
  gap: theme.spacing(0.25),
  minWidth: 0,
  flex: 1,
});

export const titleRowSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  alignItems: 'center',
  gap: theme.spacing(0.75),
  minWidth: 0,
});

export const titleSx: SxProps<Theme> = (theme: Theme) => ({
  // A bare `<button>`: no UA chrome, the card's cursor, the ring on the card.
  appearance: 'none',
  border: 0,
  padding: 0,
  margin: 0,
  background: 'none',
  textAlign: 'start',
  cursor: 'inherit',
  '&:focus-visible': { outline: 'none' },
  color: theme.vars.palette.text.secondary,
  minWidth: 0,
  overflow: 'hidden',
  textOverflow: 'ellipsis',
  whiteSpace: 'nowrap',
});

export const pinnedSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'inline-flex',
  flexShrink: 0,
  fontSize: theme.typography.headingSmall.fontSize,
  // The active-indicator role (`signal`): at least 3:1 on the card in both schemes.
  color: theme.vars.palette.icon.fill.active,
});

/** The status: a small muted badge beside the name (it used to be a large pill in the bottom corner). */
export const badgeSx: SxProps<Theme> = (theme: Theme) => ({
  flexShrink: 0,
  paddingInline: theme.spacing(0.75),
  borderRadius: theme.vars.shape.radiusSm,
  border: `1px solid ${theme.vars.palette.border.lines}`,
  color: theme.vars.palette.text.primary,
  whiteSpace: 'nowrap',
});

export const subtitleSx: SxProps<Theme> = (theme: Theme) => ({
  color: theme.vars.palette.text.primary,
  overflow: 'hidden',
  textOverflow: 'ellipsis',
  whiteSpace: 'nowrap',
});

export const descriptionSx: SxProps<Theme> = (theme: Theme) => ({
  color: theme.vars.palette.text.primary,
  wordBreak: 'break-word',
  overflow: 'hidden',
  display: '-webkit-box',
  WebkitBoxOrient: 'vertical',
  WebkitLineClamp: '2',
});

export const footerSx: SxProps<Theme> = (theme: Theme) => ({
  marginTop: 'auto',
  display: 'flex',
  alignItems: 'center',
  gap: theme.spacing(1),
  minWidth: 0,
  minHeight: '2rem',
  overflow: 'hidden',
});

export const tagsSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  alignItems: 'center',
  gap: theme.spacing(0.5),
  minWidth: 0,
  overflow: 'hidden',
});

export function tagChipSx(clickable: boolean): SxProps<Theme> {
  return (theme: Theme) => ({
    ...theme.typography.labelSmall,
    flexShrink: 1,
    minWidth: 0,
    maxWidth: '8rem',
    paddingInline: theme.spacing(0.75),
    borderRadius: theme.vars.shape.radiusPill,
    border: `1px solid ${theme.vars.palette.border.lines}`,
    color: theme.vars.palette.text.primary,
    overflow: 'hidden',
    textOverflow: 'ellipsis',
    whiteSpace: 'nowrap',
    cursor: clickable ? 'pointer' : 'inherit',
    '&:hover': clickable ? { color: theme.vars.palette.text.secondary } : {},
  });
}

export const updatedSx: SxProps<Theme> = (theme: Theme) => ({
  ...theme.typography.bodySmall,
  marginLeft: 'auto',
  flexShrink: 0,
  color: theme.vars.palette.text.primary,
  whiteSpace: 'nowrap',
});

/** The cluster: over the footer's right-hand end, on the card's own surface, so it hides the lowest-priority text while it shows. */
export const actionsSx: SxProps<Theme> = (theme: Theme) => ({
  position: 'absolute',
  right: theme.spacing(1.5),
  bottom: theme.spacing(1.5),
  zIndex: 1,
  display: 'flex',
  alignItems: 'center',
  gap: theme.spacing(0.5),
  paddingLeft: theme.spacing(1),
  background: theme.vars.palette.background.card.default,
  transition: theme.transitions.create('opacity', { duration: theme.transitions.duration.shortest }),
  [REDUCED_MOTION]: { transition: 'none' },
});
