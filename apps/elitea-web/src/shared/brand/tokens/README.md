# Default brand pack

`default.pack.json` (the pack) and `palette.augment.d.ts` (its typed MUI
members) are **hand-maintained source**. Until 2026-10-22 they were generated
by `scripts/gen-brand-tokens.mjs` from the `apps/elitea-ui` palettes
(`darkPalette.js` / `lightPalette.js`). That generator went with the
submodule, so this file keeps the reasons behind the values that did not come
straight from those palettes.

## Editing

- A new token id goes into **both** scheme records of `default.pack.json`
  (the two records must carry the same key set; otherwise a token has no
  reference geometry in one scheme) **and** into `palette.augment.d.ts`:
  `background.*` under `TypeBackground`, `text.*` under `TypeText`, the extra
  leaves of the six MUI roles (`primary`, `secondary`, `info`, `warning`,
  `error`, `success`) under `PaletteColor`, everything else under `Palette`.
- `libs/ts/client-core/src/brand/identity.ts` holds the product name and
  `brand.hue`; `defaultPack.gen.ts` there restates the hue and each scheme's
  `primary.main` / `text.button.primary` for the mobile app.
- `src/shared/brand/__tests__/defaultPackSync.test.ts` fails when any of these
  copies disagree: the two schemes' key sets, the pack vs the augmentation
  (both directions), and the pack vs client-core.

## Values that are not a verbatim copy of the old palettes

**Excluded.** `palette.mode` (scheme identity, set by `buildTheme`);
`typographyVariants.labelLarge` (dead in the old app too);
`background.moderator` and `icon.fill.select` (admin-ui/Maintenance-UI only,
or referenced but defined nowhere).

**Symmetry fills.** The old palettes had ids defined in one scheme only. Each
fill is pixel-neutral: the value is the CSS equivalent of what the missing
token rendered (nothing), or the id had no read site in that scheme.

| Token | Scheme filled | Value | Why |
| --- | --- | --- | --- |
| `primary.hover` | light | `rgba(244, 124, 255, 1)` | Light magenta hover, already the light value of `background.button.primary.hover`; read only in a dark branch |
| `text.button.auxiliary` | dark | `#83EFFF` | The dark-branch value at the same read site; read only in a light branch |
| `boxShadow.aiAnswer` | dark | `none` | Read unconditionally; undefined in dark dropped the declaration, `none` is the equivalent |
| `background.folder.shadow` | dark | `0 0 0 rgba(0, 0, 0, 0)` | Interpolated into `drop-shadow(...)`; undefined made the filter invalid (no shadow). A transparent zero shadow is equivalent and valid; `none` is not a shadow |
| `background.folder.borderGradient` | light | `none` | Used as a `background` shorthand; undefined dropped it, `none` is equivalent |

**Additions.** New ids, values copied from the old call sites so rendering
did not change:

- Brand-accent button tokens (`background.button.special.*`,
  `background.button.maxi.*`, `text.button.specialDefault|specialPressed|maxiDefault`,
  `text.button.secondaryPressed`, `background.button.iconCounter.pressed`,
  `text.button.auxiliaryDefault|Hover|Pressed`): the old button component
  branched on `palette.mode` with hard-coded cyan (dark) / magenta (light)
  ramps; each branch became one per-scheme token, so a brand pack can
  recolour it (spec §4.1 Blocker 1, `parity/brand-hue-map.md`).
- `error.*` and `success.*` as top-level MUI roles (spec §4.2): absent in the
  old palettes, so consumers fell back to MUI defaults. Values are the
  alarm-button ramp and the published-status / positive-button colours.
- `border.artifactDivider` (#6687): the Artifacts dividers, same pair as
  `border.table`, as a role of its own.

**Contrast overrides (dark scheme only).** The old palettes used
`#D71616` for error/danger in both schemes. On the dark surfaces it measures
3.56:1 against `background.default` (#0E131D) and 3.17:1 against
`background.card.default`, below WCAG AA 4.5:1 for text. The overrides keep
hue (0°) and saturation and raise lightness only:

| Token | Old | Now | Why |
| --- | --- | --- | --- |
| `icon.fill.error` | `#D71616` | `#ED4F4F` | Text colour of the TextField error helper (5.05:1) |
| `text.error` | `#D71616` | `#ED4F4F` | Same defect in the text role, fixed before it gets a consumer |
| `status.rejected` | `#D71616` | `#ED4F4F` | Used as literal text colour at many call sites |
| `background.button.alarm.default` | `#D71616` | `#ED4F4F` | Under the `alarm` button's `text.button.primary` label: 3.56:1 → 5.16:1 |
| `background.button.alarm.pressed` | `#C51111` | `#EB3A3A` | Pressed label 3.07:1 → 4.60:1, kept darker than `.hover` and `.default` so the ramp stays monotonic |

Deliberately **not** overridden: `error.main` (a fill paired with white
`error.contrastText`, 5.23:1; no lightness clears both bars at once, and
`text.error` is the text role) and `background.button.danger` (icon fill
only, 3.45:1 clears the 3:1 non-text bar). The light scheme already clears AA
with `#D71616` (5.07:1).

**Non-colour fields.** `shape.radiusPill: 9999` is the pill/circle shape the
icon-only and `maxi` buttons need: the old `borderRadius: '50%'` has no token
form, and a large fixed value clamps to the shorter side, giving a pill on a
rectangle and a circle on a square. `product.docsUrl: '/docs/'` is the
same-origin embedded docs SPA, relative so every deployment resolves it
against its own origin.
