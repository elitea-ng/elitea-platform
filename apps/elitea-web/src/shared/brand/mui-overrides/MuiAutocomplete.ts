import type { EliteaComponents } from '../theme-types';
import { typeScale } from '../typeScale';

/** `MuiAutocomplete` (R-T12). Ported verbatim from `MainTheme.js:354-363`
 * (radiusMd ≈ the baseline's `0.5rem`). */
export const MuiAutocomplete: EliteaComponents['MuiAutocomplete'] = {
  styleOverrides: {
    paper: ({ theme }) => ({
      backgroundColor: theme.vars.palette.background.secondary,
      border: `0.0625rem solid ${theme.vars.palette.border.lines}`,
      borderRadius: theme.vars.shape.radiusMd,
      boxShadow: theme.vars.palette.boxShadow.tagEditorPaper,
    }),
    // Options read like the field they complete: `bodyMedium`, not MUI's
    // stock 16px body1 (typography spec §2).
    option: ({ theme }) => typeScale(theme.typography.bodyMedium),
    noOptions: ({ theme }) => typeScale(theme.typography.bodyMedium),
  },
};
