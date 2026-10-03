import type { EliteaComponents } from '../theme-types';
import { typeScale } from '../typography';

/** `MuiTooltip` (R-T12). Ported verbatim from `MainTheme.js:342-353`. */
export const MuiTooltip: EliteaComponents['MuiTooltip'] = {
  styleOverrides: {
    tooltip: ({ theme }) => ({
      backgroundColor: theme.vars.palette.background.tooltip.default,
      color: theme.vars.palette.text.button.primary,
      ...typeScale(theme.typography.labelSmall),
      '& .MuiTooltip-arrow': {
        color: theme.vars.palette.background.tooltip.default,
      },
    }),
  },
};
