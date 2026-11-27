import type { Theme } from '@mui/material/styles';

/**
 * The neutral card surface of the Agents and Teams cards (issues 3133 and
 * 3134): the theme's card token with a hairline border, a hover lift and the
 * brand's keyboard ring, and no per-card tint (the old tile's gradient,
 * `cardGradient.ts`, is what the owner asked to drop). Every colour is a
 * token, so a brand pack and both schemes re-theme it: the surface is
 * `background.card.default` (brand guide v3: `surface` on Ink, `elevated`
 * with a border on Paper), the hairline `border.lines`, the hover border
 * `border.hover`, the hover elevation `background.card.hoverShadow` (none on
 * Ink, where elevation is a lighter step) and the ring `focus.ring`.
 *
 * `focusTarget` is the selector of the card's own button, when the card is
 * opened through one: the ring is drawn on the card while that button has
 * keyboard focus. A card without one leaves the ring to its own controls.
 *
 * Not annotated `: SxProps<Theme>`, for the reason `cardGradient.ts` gives:
 * the call sites spread it.
 */
export function cardSurfaceSx(theme: Theme, focusTarget?: string) {
  return {
    position: 'relative',
    boxSizing: 'border-box',
    borderRadius: theme.vars.shape.radiusLg,
    border: `1px solid ${theme.vars.palette.border.lines}`,
    background: theme.vars.palette.background.card.default,
    transition: theme.transitions.create(['border-color', 'box-shadow', 'transform'], { duration: theme.transitions.duration.shorter }),
    '&:hover': {
      borderColor: theme.vars.palette.border.hover,
      boxShadow: theme.vars.palette.background.card.hoverShadow,
      transform: 'translateY(-1px)',
    },
    ...(focusTarget === undefined
      ? {}
      : { [`&:has(${focusTarget}:focus-visible)`]: { outline: `0.125rem solid ${theme.vars.palette.focus.ring}`, outlineOffset: '0.125rem' } }),
    '@media (prefers-reduced-motion: reduce)': { transition: 'none', '&:hover': { transform: 'none' } },
  } as const;
}
