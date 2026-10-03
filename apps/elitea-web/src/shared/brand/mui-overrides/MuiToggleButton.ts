import type { EliteaComponents } from '../theme-types';
import { typeScale } from '../typeScale';

/**
 * `MuiToggleButton` (R-T12). Ported from `MainTheme.js`'s
 * `eliteaTabGroupButtonStyle` / `colorTabGroupButtonStyle`
 * (`apps/elitea-ui/src/[fsd]/shared/ui/tab-group-button/TabGroupButton.jsx`),
 * consumed by `shared/ui/TabButtonItem` / `TabGroupButton`. `ToggleButton`
 * has no typed `variant` prop in MUI 9.2, and this was the app's only
 * toggle-button skin — `styleOverrides.root` replaces the baseline's
 * `variant="elitea"` gate (same reasoning as `MuiCheckbox`).
 */
export const MuiToggleButton: EliteaComponents['MuiToggleButton'] = {
  styleOverrides: {
    root: ({ theme }) => ({
      border: 'none',
      display: 'flex',
      alignItems: 'center',
      justifyContent: 'center',
      marginLeft: 0,
      // Baseline `TabGroupButton.jsx:139` is `0.375rem 0.5rem` = 6px 8px.
      // `spacing(0.375)` is 3px — the vertical padding was half the baseline.
      padding: theme.spacing(0.75, 1),
      textTransform: 'none',
      fontFamily: theme.typography.fontFamily,
      ...typeScale(theme.typography.labelSmall),
      color: theme.vars.palette.text.tabButton.default,
      backgroundColor: theme.vars.palette.background.tabButton.default,
      '&&.MuiToggleButtonGroup-grouped': {
        border: 'none',
      },
      '&&.MuiToggleButtonGroup-groupedHorizontal:not(:last-of-type)': {
        borderRight: 'none',
      },
      '&&.MuiToggleButtonGroup-groupedHorizontal:not(:first-of-type)': {
        borderLeft: 'none',
        marginLeft: 0,
      },
      '&:hover': {
        color: theme.vars.palette.text.tabButton.hover,
        backgroundColor: theme.vars.palette.background.tabButton.hover,
      },
      '&.Mui-selected': {
        color: theme.vars.palette.text.tabButton.active,
        backgroundColor: theme.vars.palette.background.tabButton.active,
        '&:hover': {
          backgroundColor: theme.vars.palette.background.tabButton.active,
        },
      },
      '&:disabled': {
        color: theme.vars.palette.text.tabButton.disabled,
        backgroundColor: theme.vars.palette.background.tabButton.disabled,
      },
      '& svg': {
        width: '1rem',
        height: '1rem',
      },
    }),
  },
};
