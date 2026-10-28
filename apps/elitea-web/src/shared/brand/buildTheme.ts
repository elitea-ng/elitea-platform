import { createTheme } from '@mui/material/styles';

import {
  COLOR_SCHEME_SELECTOR,
  CSS_VAR_PREFIX,
  DEFAULT_COLOR_SCHEME,
} from './constants';
import { muiOverrides } from './mui-overrides';
import type { BrandPack } from './schema';
import { toMuiPalette } from './toMuiPalette';
import { toTypography } from './typography';

/**
 * Layout breakpoints, `MainTheme.js:92-111` verbatim. Unit T2 classified the
 * ten `prompt_list_*` rungs as class (a) — real tokens with five consuming
 * files in the baseline — so they are part of the theme, not of the fork.
 * They are deployment-invariant geometry, hence not pack fields.
 */
export const BREAKPOINT_VALUES = {
  prompt_list_xs: 0,
  prompt_list_sm: 600,
  prompt_list_full_width_sm: 1024,
  prompt_list_md: 1366,
  prompt_list_lg: 1440,
  prompt_list_xl: 1800,
  prompt_list_xxl: 2560,
  prompt_list_xxxl: 3440,
  prompt_list_xxxxl: 3840,
  prompt_list_xxxxxl: 5120,
  tablet: 1024,
  xs: 0,
  sm: 600,
  md: 900,
  lg: 1200,
  xl: 1536,
} as const;

/**
 * Where a theme's CSS variables are declared and under which prefix.
 *
 * The app theme takes the defaults (`--el-*` on `:root` / `[data-el-scheme]`).
 * A SECOND theme rendered beside it — the admin Branding page's live preview —
 * must not share those: MUI's nested `ThemeProvider` skips stylesheet
 * generation when the prefix matches the outer theme's (the preview would then
 * render the OUTER brand), and a nested theme that reuses the outer
 * `colorSchemeSelector` re-declares every `--el-*` variable on the document's
 * own `[data-el-scheme]` element, repainting the whole console with the draft.
 * A distinct prefix and selector keep a preview's variables on the preview's
 * own elements. `shared/brand/preview.ts` holds the one scope in use.
 */
export interface ThemeScope {
  readonly cssVarPrefix: string;
  readonly colorSchemeSelector: string;
  readonly rootSelector: string;
}

/**
 * Tier 2 — the one `createTheme` call (spec §4.2), on MUI 9.2.0 (decision
 * D1). Every API below was verified against context7 `/mui/material-ui/v9.2.0`
 * and, where the docs were thinner than the guarantee needed, against the
 * installed sources:
 *
 *  - `cssVariables: { cssVarPrefix, colorSchemeSelector }` — an object form is
 *    accepted alongside `true` (`createTheme.ts` ThemeOptions).
 *  - `colorSchemeSelector` containing `%s` is used verbatim with `%s`
 *    replaced by the scheme name (`@mui/system/cssVars/prepareCssVars.js`),
 *    so `[data-el-scheme="%s"]` yields `[data-el-scheme="light"|"dark"]`.
 *  - `cssVarPrefix: 'el'` renames every variable to `--el-…`
 *    (`@mui/system/cssVars/cssVarsParser.js` builds `--{prefix}-{path}` with
 *    the object path joined by `-`, key spelling preserved). Custom nested
 *    palette keys are emitted exactly like built-in ones.
 *  - `defaultColorScheme: 'dark'` makes the dark variables the `:root` set
 *    (baseline parity — `slices/settings.js:76`).
 *
 * Deviations from the §4.2 snippet, all additive and all deliberate:
 *  - `shape` carries `radiusSm|Md|Lg` besides `borderRadius`, because R-T10
 *    bans ad-hoc radii and the three named rungs must therefore be readable
 *    as `theme.vars.shape.radius*`;
 *  - `breakpoints` is set (see above);
 *  - `defaultColorScheme` is set.
 */
export function buildEliteaTheme(pack: BrandPack, scope?: ThemeScope, direction: 'ltr' | 'rtl' = 'ltr') {
  return createTheme({
    direction,
    cssVariables:
      scope === undefined
        ? {
            cssVarPrefix: CSS_VAR_PREFIX,
            colorSchemeSelector: COLOR_SCHEME_SELECTOR,
          }
        : {
            cssVarPrefix: scope.cssVarPrefix,
            colorSchemeSelector: scope.colorSchemeSelector,
            rootSelector: scope.rootSelector,
          },
    defaultColorScheme: DEFAULT_COLOR_SCHEME,
    colorSchemes: {
      light: { palette: toMuiPalette(pack.schemes.light, pack.brand, 'light') },
      dark: { palette: toMuiPalette(pack.schemes.dark, pack.brand, 'dark') },
    },
    // The variants name `text.secondary` by its CSS variable, so they must
    // carry THIS theme's prefix (see `paletteVar` in `typography.ts`).
    typography: toTypography(
      pack.typography,
      scope === undefined ? CSS_VAR_PREFIX : scope.cssVarPrefix,
    ),
    shape: {
      borderRadius: pack.shape.radiusMd,
      radiusSm: pack.shape.radiusSm,
      radiusMd: pack.shape.radiusMd,
      radiusLg: pack.shape.radiusLg,
      // [S1 Part B] radiusPill: theme-gate's R-T10 (elitea/ad-hoc-radius)
      // rejects literal `borderRadius: '50%'`/`'9999px'` — no member-
      // expression form of that literal exists. MuiButton's `icon` and
      // `maxi` variants need a true pill/circle, which none of
      // radiusSm|Md|Lg (corner-rounding rungs, not a shape) represents.
      radiusPill: pack.shape.radiusPill,
    },
    // Density is a token, not a fork: 6 vs 8 covers the whole compact mode.
    spacing: pack.shape.density === 'compact' ? 6 : 8,
    breakpoints: { values: { ...BREAKPOINT_VALUES } },
    components: muiOverrides(), // static; reads theme.vars only
  });
}

/** The theme type every consumer (providers, stories, tests) should use. */
export type EliteaTheme = ReturnType<typeof buildEliteaTheme>;
