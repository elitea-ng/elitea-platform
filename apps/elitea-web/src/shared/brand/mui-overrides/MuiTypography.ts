import type { EliteaComponents } from '../theme-types';

/**
 * `MuiTypography` (R-T12, R-C2). Not one of the baseline's 30 keys (the
 * baseline never configures this key at all, per T2 §3 — which is exactly
 * how it ends up with "1 `<h1>` and 0 `<h2>`-`<h6>` in 950 files"). Added
 * here — colour-free, `defaultProps` only — purely to satisfy R-C2:
 * `headingLarge`/`headingMedium`/`headingSmall` must render as real heading
 * elements, not `<span>`.
 *
 * The mapping follows the outline of typography spec §3, not the size order:
 * `headingLarge` is the page title (`<h1>`), and everything directly under
 * it — section, card and drawer titles (`headingSmall`), dialog and
 * empty-state titles (`headingMedium`) — is an `<h2>`. A `headingSmall`
 * `<h3>` default put every card and section title one level below a page
 * title with no `<h2>` between them, which axe reports as `heading-order`
 * on every tabbed page once `PageHeader` rendered its `<h1>`. A call site
 * nested deeper in the outline still overrides locally with
 * `component="h3"` — `variantMapping` only sets the default.
 */
export const MuiTypography: EliteaComponents['MuiTypography'] = {
  defaultProps: {
    variantMapping: {
      headingLarge: 'h1',
      headingMedium: 'h2',
      headingSmall: 'h2',
    },
  },
};
