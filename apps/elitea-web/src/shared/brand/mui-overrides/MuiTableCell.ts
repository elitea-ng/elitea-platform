import type { EliteaComponents } from '../theme-types';
import { typeScale } from '../typeScale';

/**
 * `MuiTableCell` (R-T12; typography spec §2). One table type scale for the
 * app and the admin console:
 *
 *  - head: `labelMedium` in `text.primary`, the DIM text token (this app's
 *    text tokens are inverted: `text.primary` is the grey, `text.secondary`
 *    the strong one). Colour, not size or weight, separates the header row
 *    from the cells.
 *  - body: `bodyMedium`.
 *  - footer: `bodySmall`.
 *
 * MUI's own cell reads `body2` (now aliased to `bodyMedium`) and gives the
 * head a fixed 24px leading; this states the roles instead of inheriting
 * them.
 */
export const MuiTableCell: EliteaComponents['MuiTableCell'] = {
  styleOverrides: {
    head: ({ theme }) => ({
      ...typeScale(theme.typography.labelMedium),
      color: theme.vars.palette.text.primary,
    }),
    body: ({ theme }) => typeScale(theme.typography.bodyMedium),
    footer: ({ theme }) => typeScale(theme.typography.bodySmall),
  },
};
