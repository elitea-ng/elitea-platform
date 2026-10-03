import type { EliteaComponents } from '../theme-types';
import { typeScale } from '../typeScale';

/**
 * `MuiTablePagination` (R-T12). Ported from `MainTheme.js:252-273`. The
 * baseline's bare `fontSize: '0.75rem'` (root, menuItem) is R-T11's exact
 * target case; `labelSmall`'s size (also `0.75rem`, same rung of the modular
 * scale) replaces it via a member-expression read, which is not a literal
 * and so is not the "invented, untracked size" the rule exists to catch.
 */
export const MuiTablePagination: EliteaComponents['MuiTablePagination'] = {
  styleOverrides: {
    root: ({ theme }) => ({
      fontSize: theme.typography.labelSmall.fontSize,
      color: theme.vars.palette.text.default,
      '& .MuiTablePagination-select.MuiSelect-standard': {
        color: theme.vars.palette.text.default,
      },
    }),
    selectLabel: ({ theme }) => ({
      ...typeScale(theme.typography.labelSmall),
      color: theme.vars.palette.text.button.disabled,
    }),
    displayedRows: ({ theme }) => ({
      ...typeScale(theme.typography.labelSmall),
      color: theme.vars.palette.text.default,
    }),
    menuItem: ({ theme }) => ({
      fontSize: theme.typography.labelSmall.fontSize,
    }),
  },
};
