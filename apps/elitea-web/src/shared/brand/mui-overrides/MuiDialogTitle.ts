import type { EliteaComponents } from '../theme-types';
import { typeScale } from '../typography';

/**
 * `MuiDialogTitle` (R-T12; typography spec §2). Every dialog title is
 * `headingMedium`. Before this key, MUI's DialogTitle rendered its stock
 * `h6` (20px/500) and call sites patched it to 14, 16 or 20.
 *
 * The colour is set EXPLICITLY to `text.secondary` (the STRONG text token;
 * this app's text tokens are inverted). `typeScale` deliberately never
 * copies a heading variant's colour, so a title that should read as a
 * heading says so here rather than inheriting it by accident.
 */
export const MuiDialogTitle: EliteaComponents['MuiDialogTitle'] = {
  styleOverrides: {
    root: ({ theme }) => ({
      ...typeScale(theme.typography.headingMedium),
      color: theme.vars.palette.text.secondary,
    }),
  },
};
