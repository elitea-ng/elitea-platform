import type { EliteaComponents } from '../theme-types';
import { typeScale } from '../typeScale';

/**
 * `MuiInput` (R-T12). Ported from `input/textFieldVariants.js`'s
 * `eliteaInputVariants` — colours the raw `<input>` from `text.secondary`.
 * `Input`'s `variant` is a single fixed literal (`'standard'`) with no
 * `PropsVariantOverrides` mechanism, so `styleOverrides.root` (applies
 * unconditionally) is the direct equivalent of the baseline's one-entry
 * `variants` array.
 */
export const MuiInput: EliteaComponents['MuiInput'] = {
  styleOverrides: {
    root: ({ theme }) => ({
      '& input': {
        // Baseline `textFieldVariants.js:6-16` spreads `bodyMedium` here
        // (0.875rem/400/1.5rem). Without it the input inherited MUI's stock
        // 1rem body font while its label and helper text used the brand
        // scale, so every standard input sat a step too large.
        ...typeScale(theme.typography.bodyMedium),
        color: theme.vars.palette.text.secondary,
      },
    }),
  },
};
