import type { EliteaComponents } from '../theme-types';

/**
 * `MuiChip` — canonical elitea-ui shape (`MainTheme.js:217-227`), token-wired.
 *
 * Unit T2 §3 classified admin-ui's chip as class (b): it branches on the
 * palette mode in JavaScript (an R-T2 violation) and adds a transparent
 * `outlined` rework plus a `deleteIcon` override. None of that is
 * ported — the branch is precisely what the CSS-variable layer exists to
 * delete, and the canonical two-slot override below is scheme-correct by
 * construction.
 */
export const MuiChip: EliteaComponents['MuiChip'] = {
  styleOverrides: {
    root: ({ theme }) => ({
      background: theme.vars.palette.background.avatar,
      // MEASURED, reference source: `apps/elitea-ui/src/MainTheme.js:144-155`
      // sets `background` and `outlined` and NO `borderRadius`, so every chip
      // in the baseline renders MUI's own default — `borderRadius: 32 / 2`,
      // that is 16px. `shape.radiusLg` is 16 (`tokens/default.pack.json:23`),
      // so this states the same number as a token instead of inheriting a
      // library constant that a MUI upgrade can move.
      //
      // It goes on `root` and not on a state slot. `.Mui-selected`/
      // `.MuiChip-clickable` carry no radius of their own, so no `&&`
      // specificity lift is needed here; adding one would beat a call site's
      // own `sx`, which two components legitimately set (issue 841).
      borderRadius: theme.vars.shape.radiusLg,
    }),
    outlined: ({ theme }) => ({
      background: theme.vars.palette.background.eliteaDefault,
      color: theme.vars.palette.text.secondary,
    }),
  },
};
