import type { EliteaComponents } from '../theme-types';
import { typeScale } from '../typography';

/** `MuiBadge` (R-T12). Ported verbatim from `MainTheme.js:329-341`
 * (radiusMd ≈ the baseline's `0.5rem`). */
export const MuiBadge: EliteaComponents['MuiBadge'] = {
  styleOverrides: {
    badge: ({ theme }) => ({
      ...typeScale(theme.typography.labelSmall),
      color: theme.vars.palette.text.secondary,
      height: '1rem',
      minWidth: '1rem',
      borderRadius: theme.vars.shape.radiusMd,
      padding: `0 ${theme.spacing(0.5625)}`,
      background: theme.vars.palette.background.tabButton.active,
    }),
  },
};
