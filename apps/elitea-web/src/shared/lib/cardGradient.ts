import type { Theme } from '@mui/material/styles';

/**
 * Ported from `apps/elitea-ui/src/utils/cardStyles.js`'s
 * `getCardGradientStyles`/`getCardGradientBorderBefore`/
 * `getCardGradientHover` — the gradient-border card treatment
 * `ApplicationCatalogCard` (and several other old-app card components)
 * shares. Not in `shared/`: this exact helper was not one of unit S3's two
 * porting targets (`utils.jsx` + `constants.js`), so it has no shared/lib
 * home yet; kept local to this slice per spec §3.3 (`lib/`: "pure helpers
 * ... local to the slice"). Reads `theme.vars.palette.*` (R-T7), not a bare
 * `palette` parameter like the baseline.
 *
 * Deliberately NOT annotated `: SxProps<Theme>` — `SxProps<Theme>` is a
 * union that includes an ARRAY branch, and spreading a union-typed value at
 * a call site (`{...cardGradientSx(theme), ...moreStyles}`, this file's own
 * consumer) collapses to an unusable array-like structural type. Letting
 * TypeScript infer the concrete object literal type here (this function
 * only ever returns one shape) is what keeps that spread well-typed.
 *
 * Two deviations from the baseline's literal CSS, both to satisfy tokens
 * the theme-gate lint enforces (R-T10/R-T1) and both already established by
 * `shared/ui/GradientIconWrapper` (unit S1) for the exact same gradient-
 * border-via-mask technique:
 *  - `borderRadius: '0.75rem'` (12px) has no exact match among
 *    `radiusSm|Md|Lg` (4/8/16px) — `radiusLg` is used, the same choice
 *    `GradientIconWrapper` made for its own rounded frame.
 *  - The mask gradients use `currentColor` instead of a raw `#fff`
 *    literal — masks only ever consume the alpha channel of the colour
 *    they're given, so any fully-opaque colour paints an identical mask;
 *    `currentColor` (always opaque here) avoids a raw literal for a value
 *    CSS never actually renders (R-T1).
 */
export function cardGradientSx(theme: Theme, options: { enableHover?: boolean } = {}) {
  const { enableHover = true } = options;

  return {
    position: 'relative',
    borderRadius: theme.vars.shape.radiusLg,
    border: 'none',
    background: theme.vars.palette.background.card.gradientDark,
    '&::before': {
      content: '""',
      position: 'absolute',
      inset: 0,
      borderRadius: theme.vars.shape.radiusLg,
      padding: '0.0625rem',
      background: theme.vars.palette.border.cardsOutlinesGradient,
      WebkitMask: 'linear-gradient(currentColor 0 0) content-box, linear-gradient(currentColor 0 0)',
      maskComposite: 'exclude',
      WebkitMaskComposite: 'xor',
      pointerEvents: 'none',
    },
    ...(enableHover
      ? {
          '&:hover': {
            background: theme.vars.palette.background.card.hover,
            '&::before': {
              background: theme.vars.palette.background.card.hoverBorderGradient,
            },
            boxShadow: theme.vars.palette.background.card.hoverShadow,
          },
        }
      : {}),
  };
}
