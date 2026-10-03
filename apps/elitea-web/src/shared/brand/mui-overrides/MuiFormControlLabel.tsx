import type { EliteaComponents } from '../theme-types';
import { typeScale } from '../typeScale';

/**
 * `MuiFormControlLabel` (R-T12). A checkbox / switch / radio label is
 * `bodyMedium` — running text beside a control, the same size as the input
 * text next to it (typography spec §2).
 *
 * All colours read from `theme.vars.palette.*` to support white-label branding.
 */
export const MuiFormControlLabel: EliteaComponents['MuiFormControlLabel'] = {
  styleOverrides: {
    label: ({ theme }) => ({
      ...typeScale(theme.typography.bodyMedium),
    }),
  },
};
