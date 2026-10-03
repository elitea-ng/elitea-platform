import type { EliteaComponents } from '../theme-types';
import { typeScale } from '../typography';

/**
 * `MuiTab` (R-T12). Ported from
 * `apps/elitea-ui/src/[fsd]/shared/ui/tabs/BaseTab.jsx`'s
 * `eliteaTabVariants` / `MuiTabStyles`. `Tab` has no typed `variant` prop in
 * MUI 9.2 (unlike its parent `Tabs`, which does), and this was the app's
 * only tab skin, so `styleOverrides.root` (applies unconditionally, still
 * receives `ownerState` for the icon-only-tab width rule) replaces the
 * baseline's `defaultProps.variant: 'elitea'` + `variants` gate.
 *
 * Deviations from the baseline, both found by this unit's own Storybook
 * a11y run (`a11y.test: 'error'` — the baseline's `'todo'` could never have
 * caught either):
 *  - The default-state colour targeted `&.MuiTab-textColorPrimary`, gated
 *    on `Tabs`' `textColor="primary"`. In MUI 9.2 `Tabs`' default
 *    `textColor` is `'inherit'`, not `'primary'` (the baseline never set
 *    `textColor` either, so this is a genuine MUI-version behaviour change,
 *    not a baseline choice) — so the gated rule never matched, and every
 *    tab silently fell back to MUI's own `inherit` colour. Fixed by setting
 *    colour unconditionally on `root` AND repeating it on
 *    `&.MuiTab-textColorInherit` (equal-specificity tiebreak against Tab's
 *    own base styles — the unconditional form alone still lost in
 *    practice).
 *  - MUI's base `Tab` styles additionally set `opacity: 0.6` on the
 *    unselected state (`textColorInherit`'s default), which — even after
 *    the colour fix above — re-composited `background.tab.default`
 *    (`#A9B7C1`, contrast-safe on its own) down to an *effective* rendered
 *    colour of `#6B757F` against the dark scheme's background: 3.96:1,
 *    short of WCAG AA's 4.5:1. `opacity: 1` removes that invisible
 *    second dimming step; the disabled state already gets its own
 *    contrast-checked colour below rather than relying on opacity to read
 *    as "dimmed".
 */
export const MuiTab: EliteaComponents['MuiTab'] = {
  styleOverrides: {
    root: ({ theme, ownerState }) => {
      const isIconOnly = ownerState.label === '' || ownerState.label == null;
      return {
        // Tabs are `labelMedium` everywhere (typography spec §2) — MUI's
        // stock tab font is the 14px uppercase `button` variant.
        ...typeScale(theme.typography.labelMedium),
        padding: `${theme.spacing(1)} ${theme.spacing(2)} ${theme.spacing(1)} ${theme.spacing(2)}`,
        borderRadius: `${theme.vars.shape.radiusMd} ${theme.vars.shape.radiusMd} 0 0`,
        minHeight: 0,
        textTransform: 'none',
        flex: '0 0 auto',
        whiteSpace: 'nowrap',
        overflow: 'visible',
        textOverflow: 'clip',
        maxWidth: 'none',
        ...(isIconOnly ? { minWidth: '3.5rem' } : {}),
        color: theme.vars.palette.background.tab.default,
        opacity: 1,
        '&.MuiTab-textColorInherit': {
          color: theme.vars.palette.background.tab.default,
          opacity: 1,
        },
        '& .MuiSvgIcon-root': {
          color: 'inherit',
          width: '1rem',
          height: '1rem',
        },
        '&.Mui-selected': {
          color: theme.vars.palette.background.tab.active,
        },
        '&:hover:not(.Mui-selected):not(.Mui-disabled)': {
          color: theme.vars.palette.background.tab.hover,
        },
        '&.Mui-disabled': {
          color: theme.vars.palette.background.tab.disabled,
        },
      };
    },
  },
};
