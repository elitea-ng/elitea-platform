import type { EliteaComponents } from '../theme-types';
import { typeScale } from '../typeScale';

/**
 * `MuiSelect` (R-T12). Ported from `MainTheme.js:235-242` (the `select`
 * slot colour) plus `select/singleSelectVariants.js`'s
 * `eliteaSingleSelectVariants` (the baseline's only `Select` variant,
 * `'standard'`), which resolves the underline/label state colours `SingleSelect`
 * depends on.
 */
export const MuiSelect: EliteaComponents['MuiSelect'] = {
  styleOverrides: {
    select: ({ theme }) => ({
      color: theme.vars.palette.text.secondary,
    }),
  },
  variants: [
    {
      props: { variant: 'standard' },
      style: ({ theme }) => ({
        '&.MuiInput-underline:before': {
          borderBottom: `0.0625rem solid ${theme.vars.palette.border.lines}`,
        },
        '&:not(.Mui-error).MuiInput-underline.Mui-focused:after': {
          borderBottom: `0.0625rem solid ${theme.vars.palette.primary.main}`,
        },
        '&:not(.Mui-error).MuiInput-underline.Mui-disabled:before': {
          borderBottom: `0.0625rem solid ${theme.vars.palette.border.lines}`,
        },
        '&.Mui-error.MuiInput-underline:before, &.Mui-error.MuiInput-underline:after': {
          borderBottom: `0.0625rem solid ${theme.vars.palette.icon.fill.error}`,
        },
        '& .MuiSelect-select': {
          color: theme.vars.palette.text.select.selected.primary,
          // Select value text is `bodyMedium`, like every other input value
          // (typography spec §2) — it was 16px beside 14px fields.
          ...typeScale(theme.typography.bodyMedium),
        },
        '& .MuiSelect-select:focus': {
          backgroundColor: 'transparent',
        },
        '& .MuiInput-input.Mui-disabled, &.Mui-disabled .MuiSelect-select': {
          color: theme.vars.palette.text.default,
          WebkitTextFillColor: theme.vars.palette.text.default,
          cursor: 'not-allowed',
        },
        '& fieldset': {
          border: 'none',
          outline: 'none',
        },
      }),
    },
  ],
};
