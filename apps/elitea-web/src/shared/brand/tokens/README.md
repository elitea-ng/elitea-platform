# Default brand pack

`default.pack.json` (the pack) and `palette.augment.d.ts` (its typed MUI
members) are **hand-maintained source**. Until 2026-10-22 they were generated
by `scripts/gen-brand-tokens.mjs` from the `apps/elitea-ui` palettes
(`darkPalette.js` / `lightPalette.js`). That generator went with the
submodule. Since brand guide v3 the values come from client-core's
`tokens.json` (below); this file keeps the mapping decisions.

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

## Roles

The 27 roles of `tokens.json` (`schemes.dark` / `schemes.light`) are ids
`role.<name>` in both scheme records, with the `tokens.json` values verbatim
(ADR-0062 decision 1). New shared code reads a role
(`theme.vars.palette.role.textMuted`, `--el-palette-role-textMuted`); the
legacy ids below stay for the call sites that read them. Change a role here
only together with `tokens.json`: `__tests__/roles.test.ts` fails when they
differ.

`legacy-roles.json` maps a role to its generic legacy id,
`{ "<role>": ["<legacy id>"] }` (ADR-0062 decision 1.4). A role has exactly one
id, or no entry. A role cannot hold two tuned colours, so a second id would lose
one of them.

`resolveScheme` gives a role the first value that the pack tunes: the role id
first, then its legacy id. A value is tuned when it differs from the value that
the id gets unstated (its default, re-hued). So a pin at the default under
another hue is tuned too. `brand.onBrand` gives its colour to `role.onPrimary`,
and in light to `role.onSignal`, when the pack does not tune that id. It always
gives its colour to `primary.contrastText`.

Shared code reads `role.textMuted` for muted text, not `text.default`. It reads
`role.signal` for the focus ring, not `focus.ring` (`tokens.json` `focus.color`
is `signal`). `text.default` and `focus.ring` are not on the map. From PR 4
(#3092), theme-gate check 10 counts reads of the ids on the map. The stage 4
script (ADR-0062 Rollout) rewrites only those ids.

To change the map:

- Add an id by hand. Its meaning must be the role.
- Its colour here must be the role's colour in both schemes.
- Add an upgrade note for each change of the map.

`__tests__/roles.test.ts` holds the ids, the one list per id, the colours and
the one id per role. Review holds the meaning and the upgrade note. A change of
a role's value also changes its mapped legacy id. That is a visible change
(ADR-0062 decision 5). What this means for operators:
`docs/upgrading/2026-11-26-3091-colour-roles-hand-tuned-legacy-ids.md`.

## Where the values come from

The pack follows the Elitea AI brand guide v3: every colour is a token of
`libs/ts/client-core/src/brand/tokens.json` (dark scheme = Ink, light scheme =
Paper), stated as its hex, or as an alpha of a token hex where a role needs a
wash. Do not add a colour that is not in `tokens.json`; pick the token with
the same job. The guide is published at
<https://elitea-ng.github.io/elitea-platform/brand/>.

The token ids keep their old names (component code reads them), so a name
does not always say what the value now is. The mapping decisions:

| Old role | Token now | Notes |
| --- | --- | --- |
| Accent / primary action (`primary.*`, `background.button.{primary,neutral,positive,maxi}.*`, switch on, checkbox and radio checked, tag / chip selected) | `primary` / `primaryHover` / `primaryPressed`, label `onPrimary` | One action colour in both schemes. `text.button.primary` is white in both schemes |
| Soft accent (`background.button.special.*`, `split.*`, selected rows, drawer / conversation selection, drag target) | `selected` (and an alpha of `primary` for hover / pressed) | No per-scheme magenta / cyan washes |
| Links, active indicators, focus (`text.link`, `info.main`, tab indicators, `icon.fill.active`, `status.draft`, `focus.ring`) | `signal` | Cyan on Ink, indigo on Paper. `focus.ring` (added in v3) is the one keyboard focus colour: `MuiCssBaseline` draws a 2px ring with a 2px offset |
| AI presence (`aiAssistant.*`, `capability.*`, magic assistant, agent hub icon, `nodeColors.llm|agent`) | `ai` (icons, fills, alpha washes) and `aiText` (text) | Violet marks AI only |
| Canvas / surfaces (`background.default`, `card.*`, `secondary`, `modal.simple`, `sideBar`, …) | `bg` → `surface` → `elevated` | Dark: page `bg`, cards `surface`, raised `elevated`. Light: page `bg`, cards `elevated` with a `border`, insets `surface` |
| Text (`text.secondary` = strong, `text.primary` / `text.default` = muted) | `text` / `textMuted` / `textDisabled` | The old names are inverted (`text.primary` is the MUTED role); kept for the call sites |
| Hairlines (`border.lines`, `table`, dividers, `cardsOutlines`) | `border` | |
| Control boundaries and hover borders (`border.hover`, `checkbox.default`, `radio.default`, switch off track, `suggestionChip.border`) | `borderStrong` | ≥3:1 (WCAG 1.4.11). Inputs now draw their resting outline with `border.hover` and their hover outline with `border.inputHover` (`textMuted`) |
| Alarm button, `error.main` | `dangerFill` (hover / pressed and `error.dark`: the Paper `danger` value) | White label on it in both schemes |
| Error / warning / success text and icons | `danger` / `warning` / `success` (washes: alpha of the same) | Amber for warnings; orange is never a warning |
| `background.deprecated` + `text.deprecated` | Ink-scheme `warning` fill with `bg` Ink text, both schemes | Amber on Paper is too dark for a fill with dark text and too light for white |
| `success.contrastText` | `onSignal` (Ink) on dark, `onPrimary` (white) on light | The Ink success green does not carry white text |
| Every former gradient (cards, banners, agent modal, welcome, side bar, resource cards, tour prompt, icon wells, borders) | the flat token of the same job | No gradients in the pack |
| Scroll / image fades (`conversation{Top,Bottom}Cover`, `imageAttachment`) | `transparent` | A flat fade would paint an opaque band |
| Tooltips | inverted: `text` fill with `bg` label | |
| Shadows (`boxShadow.*`, card hover shadow) | Paper: the guide's light elevation; Ink: `none` | Dark elevation is a lighter surface step, never a shadow |
| Pipeline node colours (`nodeColors.*`) | `elevated` (dark) / `surface` (light); `llm` and `agent` an alpha of `ai` | No per-type accent hues |
| Help Center resource cards (`background.resourceCard.<colour>.*`) | the same neutral values for every colour | The page no longer reads per-card colours |

**Symmetry.** Both scheme records carry the same 435 ids; a value that has
no meaning in one scheme (for example `background.folder.shadow` on Ink) is
the pixel-neutral equivalent (`0 0 0 rgba(0, 0, 0, 0)`).

**Non-colour fields.** Typography: `fontFamily` is the Manrope stack,
`fontFamilyDisplay` (optional, added in v3; headings `h1`–`h6` and the
`heading*` variants) the Sora stack, `fontFamilyMono` the JetBrains Mono
stack; `fontFaces` declares the three vendored variable faces
(`./brand/fonts/*.woff2`, served from `public/brand/fonts/`, document-relative
like the logo placeholders; nginx's brand alias resolves them from any route
depth). Shape follows `tokens.json` (`radiusSm` 6, `radiusMd` 10,
`radiusLg` 14, `radiusPill` 9999); the pill value is the pill/circle shape the
icon-only and `maxi` buttons need. `product.docsUrl: '/docs/'` is the
same-origin embedded docs SPA.
