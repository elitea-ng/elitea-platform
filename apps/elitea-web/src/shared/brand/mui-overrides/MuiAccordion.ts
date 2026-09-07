import type { EliteaComponents } from '../theme-types';

/**
 * `MuiAccordion` (R-T12) — NOT one of the baseline's 30 ported keys; the
 * baseline (`apps/elitea-ui`, `frontends/EliteaUI` — both read) never
 * styles `Accordion` at all, so it renders with MUI's plain, unthemed
 * defaults.
 *
 * MEASURED DIRECTLY against production (`https://next.elitea.ai`,
 * `getComputedStyle` on live `.MuiAccordion-root`/`-Summary-root`/
 * `-Details-root` nodes on `/app/settings/project-general` and `/app/chat`
 * — not inferred from source, which is what led two earlier drafts of this
 * file astray):
 *
 *  - `border-radius: 4px`, on every accordion. `border: 0px none` — NO
 *    border, contrary to this app's usual "bordered card" house style
 *    (`MuiMenu.ts`/`MuiList.ts`). `box-shadow: none` (already set by
 *    `StyledAccordion.tsx`'s own `sx`, so no theme-level change needed).
 *  - A STACKED sibling list (`/app/chat`'s folder accordions, several
 *    `.MuiAccordion-root` as direct siblings) shows MUI's own default
 *    `:first-of-type`/`:last-of-type` partial rounding UNCHANGED — a
 *    middle item is square, only the group's own outer edges are rounded.
 *    Production does not "fix" this; it is not a defect to correct here.
 *  - `AccordionSummary`/`AccordionDetails` carry `border-radius: 0` and a
 *    transparent background on every measured node — no override needed.
 *
 * `4px` is this app's `theme.vars.shape.radiusSm` (`default.pack.json`:
 * `radiusSm: 4, radiusMd: 8`). `buildTheme.ts` sets the AMBIENT
 * `theme.shape.borderRadius` (what MUI's own un-overridden `Accordion.js`
 * reads for its `:first-of-type`/`:last-of-type` rule) to `radiusMd` (8px)
 * — a real, single-token mismatch, and the entire gap: every accordion in
 * this app currently renders 8px corners where production renders 4px.
 *
 * Fixed by re-declaring MUI's own two rules verbatim, substituting
 * `radiusSm` for the ambient `shape.borderRadius` they'd otherwise read —
 * same selectors, same specificity, same source-order-wins precedence over
 * MUI's inline base styles, so the gating (only a stack's true first/last
 * member rounds) is preserved exactly, not flattened. Nothing else
 * (`border`, `overflow`, a `.Mui-expanded` rule) is added: production has
 * none of it, and MUI's own `&.Mui-expanded` rule touches only `margin`
 * (`Accordion.js`), so there is no specificity fight to pre-empt here.
 */
export const MuiAccordion: EliteaComponents['MuiAccordion'] = {
  styleOverrides: {
    root: ({ theme }) => ({
      '&:first-of-type': {
        borderTopLeftRadius: theme.vars.shape.radiusSm,
        borderTopRightRadius: theme.vars.shape.radiusSm,
      },
      '&:last-of-type': {
        borderBottomLeftRadius: theme.vars.shape.radiusSm,
        borderBottomRightRadius: theme.vars.shape.radiusSm,
      },
    }),
  },
};
