import type { EliteaComponents } from '../theme-types';
import { typeScale } from '../typeScale';

/**
 * `MuiFormHelperText` (R-T12). Ported from `MainTheme.js:179-187`.
 *
 * Deviation from the baseline: `theme.palette.status.rejected` is not a
 * text-contrast-tuned role (`status.rejected` backs status pills/badges).
 *
 * [S1-E follow-up] `text.error` was the first replacement tried here. At the
 * time, it was `#D71616` in BOTH colour schemes (`shared/brand/tokens/
 * default.pack.json`), the same non-text-tuned red `CharacterCounter.tsx`'s
 * own doc comment flags for `error.main`: 3.55:1 against the dark scheme's
 * background at this text size, short of WCAG AA's 4.5:1. Any `Mui-error`-
 * state `FormHelperText` — every validation message `CommonNumberField`'s
 * (and any future field's) text/number inputs render — inherited that exact
 * failure; caught by this unit's own Storybook a11y gate (`a11y.test:
 * 'error'`) on `CommonNumberField`'s `Invalid` story. `text.warningText` is
 * the token `CharacterCounter` and `BannerMessage`'s error variant both
 * already use instead, precisely because it IS tuned per scheme
 * (`rgba(215,22,22,1)` light / `rgba(255,223,223,1)` dark) — applying the
 * same established fix here instead of inventing a third approach.
 *
 * (A concurrent brand-pack fix has since raised dark-scheme `text.error`
 * itself to `#ED4F4F`, 5.16:1 — passing on its own. `text.warningText` is
 * kept regardless: it is the established, scheme-tuned convention this
 * codebase already uses for error-adjacent text, and matching it keeps this
 * file consistent with `CharacterCounter`/`BannerMessage` rather than
 * introducing a second passing-but-different red for the same role.)
 */
export const MuiFormHelperText: EliteaComponents['MuiFormHelperText'] = {
  styleOverrides: {
    root: ({ theme }) => ({
      // Helper and error text is `bodySmall` — MUI's stock caption carries a
      // 0.4px tracking no other text in the app has.
      ...typeScale(theme.typography.bodySmall),
      '&.Mui-error': {
        color: theme.vars.palette.text.warningText,
      },
    }),
  },
};
