import type { EliteaComponents } from '../theme-types';
import { typeScale } from '../typeScale';

/**
 * `MuiOutlinedInput` (R-T12). Provides background color and border styling
 * for outlined `TextField` variants that the baseline `MuiTextField` override
 * (which only covers `'standard'`) does not address.
 *
 * All colour tokens read from `theme.vars.palette.*` to support white-label
 * branding.  No internal MUI selectors are used — every rule targets the
 * component's own root or well-known state classes (`Mui-focused`, `Mui-error`).
 */
export const MuiOutlinedInput: EliteaComponents['MuiOutlinedInput'] = {
  styleOverrides: {
    root: ({ theme }) => {
      const { palette, shape } = theme.vars;
      return {
        backgroundColor: palette.background.userInputBackground,
        borderRadius: shape.radiusMd,
        ...typeScale(theme.typography.bodyMedium),
        color: palette.text.secondary,
        '& .MuiOutlinedInput-notchedOutline': {
          borderColor: palette.border.lines,
        },
        // The notch is sized by an invisible copy of the label inside the
        // `<legend>`, which MUI renders at a hard-coded `0.75em` of THIS
        // root's font — 10.5px at 14px input text, narrower than the shrunk
        // label (`MuiInputLabel.ts`: labelMedium × k = rung −1, 12px). The
        // label shrinks onto rung −1 at weight 500, which is exactly
        // `labelSmall`, so the legend reads that variant's size and weight
        // and the gap always matches the label for every pack.
        '& .MuiOutlinedInput-notchedOutline legend': {
          fontSize: theme.typography.labelSmall.fontSize,
          fontWeight: theme.typography.labelSmall.fontWeight,
          letterSpacing: theme.typography.labelSmall.letterSpacing,
        },
        '&:hover .MuiOutlinedInput-notchedOutline': {
          borderColor: palette.border.lines,
        },
        '&.Mui-focused .MuiOutlinedInput-notchedOutline': {
          borderColor: palette.primary.main,
          borderWidth: '0.0625rem',
        },
        '&.Mui-error .MuiOutlinedInput-notchedOutline': {
          borderColor: palette.icon.fill.error,
        },
      };
    },
  },
};
