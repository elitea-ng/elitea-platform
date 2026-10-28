import type { EliteaComponents } from '../theme-types';
import { typeScale } from '../typeScale';

/** `MuiTooltip` (R-T12). Ported verbatim from `MainTheme.js:342-353`. */
export const MuiTooltip: EliteaComponents['MuiTooltip'] = {
  styleOverrides: {
    tooltip: ({ theme }) => ({
      backgroundColor: theme.vars.palette.background.tooltip.default,
      // `text.tooltip` is the tooltip's own on-colour (Paper on Ink in light, Ink on
      // Paper in dark); `text.button.primary` is white on Indigo in both schemes.
      color: theme.vars.palette.text.tooltip,
      ...typeScale(theme.typography.labelSmall),
      '& .MuiTooltip-arrow': {
        color: theme.vars.palette.background.tooltip.default,
      },
    }),
  },
};
