import type { EliteaComponents } from '../theme-types';
import { typeScale } from '../typeScale';

/**
 * `MuiFormLabel` (R-T12; typography spec §2). A form label is `labelMedium`,
 * whether it is a floating `InputLabel` or a bare `<FormLabel>` over a radio
 * group, toggle group or switch row.
 *
 * Without this key a bare `FormLabel` read MUI's `body1` alias
 * (`bodyMedium`, weight 400), so a radio-group label rendered lighter than
 * every TextField label beside it. `InputLabel` is a styled `FormLabel`, so
 * it gets this root too; `MuiInputLabel.ts` restates the same variant and
 * adds the shrink geometry.
 */
export const MuiFormLabel: EliteaComponents['MuiFormLabel'] = {
  styleOverrides: {
    root: ({ theme }) => typeScale(theme.typography.labelMedium),
  },
};
