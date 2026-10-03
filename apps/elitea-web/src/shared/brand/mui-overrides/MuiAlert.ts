import type { EliteaComponents } from '../theme-types';
import { typeScale } from '../typeScale';

/**
 * `MuiAlert` (R-T12). **Re-authored, not ported** — see OWNERSHIP.md note 1.
 * The baseline (`MainTheme.js:276-299`) targets `styleOverrides.filledSuccess`
 * /`filledError`/`filledWarning`/`filledInfo` — combined slot keys that no
 * longer exist on `AlertClasses` in MUI 9.2 (verified against the installed
 * `alertClasses.d.ts`: `filled` and `colorSuccess`/`colorError`/
 * `colorWarning`/`colorInfo` are now separate classes, both applied to the
 * root). The `variant`+`color` `variants` matcher below is the 9.2-native
 * equivalent and — as a beneficial side effect — deletes the CSS named
 * colours (`'green'`/`'red'`/`'orange'`) the baseline used, which is exactly
 * what R-T1 exists to catch (T2 §3 calls this a shared elitea-ui/admin-ui
 * defect). `contrastText` (not a hard-coded `'white'`) keeps each filled
 * surface's text colour correct for a brand pack whose role colours are not
 * necessarily white-on-dark.
 *
 * Matches on `severity`, not `color`: verified against the installed
 * `@mui/material/Alert/Alert.js` — MUI's OWN built-in filled/standard/
 * outlined variants (lines ~63-103) key off a runtime-only `colorSeverity`
 * ownerState field computed as `color || severity` (Alert.js:177), which is
 * not part of the public `AlertProps`/`AlertOwnerState` types, so a
 * same-shape override here can't type-check against it directly. Matching
 * on `color` (the literal prop) instead, as this file originally did, only
 * fires when a caller passes BOTH `severity` and an explicit `color` —
 * every real usage in this app (see `__tests__/surfaces.tsx`, the only
 * current consumer) passes `severity` alone, so `color` stays `undefined`
 * and none of these variants ever matched: caught by a real functions-
 * coverage regression (0% on this file), not by inspection. `severity` is
 * a typed `AlertProps` field and is what `colorSeverity` resolves to for
 * every current and, per that same grep, every historical call site.
 */
export const MuiAlert: EliteaComponents['MuiAlert'] = {
  styleOverrides: {
    // Alert copy is running text: `bodyMedium` (typography spec §2).
    message: ({ theme }) => typeScale(theme.typography.bodyMedium),
  },
  variants: [
    {
      props: { variant: 'filled', severity: 'success' },
      style: ({ theme }) => ({
        backgroundColor: theme.vars.palette.success.main,
        color: theme.vars.palette.success.contrastText,
      }),
    },
    {
      props: { variant: 'filled', severity: 'error' },
      style: ({ theme }) => ({
        backgroundColor: theme.vars.palette.error.main,
        color: theme.vars.palette.error.contrastText,
      }),
    },
    {
      props: { variant: 'filled', severity: 'info' },
      style: ({ theme }) => ({
        backgroundColor: theme.vars.palette.info.main,
        color: theme.vars.palette.info.contrastText,
      }),
    },
    {
      props: { variant: 'filled', severity: 'warning' },
      style: ({ theme }) => ({
        backgroundColor: theme.vars.palette.warning.main,
        color: theme.vars.palette.warning.contrastText,
      }),
    },
  ],
};
