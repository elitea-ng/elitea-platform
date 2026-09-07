# `mui-overrides/` — key ownership (R-T12)

MUI `styleOverrides` and `variants` exist **only** in this directory, one file per
`Mui*` key, each read exclusively through `theme.vars.*`. Theme-gate check 4
(`no MUI internal selectors outside the override package`) and the
`elitea/no-mui-internal-selector` override in `.oxlintrc.json` are scoped to this
path and nowhere else.

Unit **T1** established the structure and wired the two keys its contract test
needs. Unit **S1** (`src/shared/ui/**`, Wave 1) owns the rest: it ports the
baseline's component surface and must add one file per key here, plus a Storybook
story per file (R-T12).

The baseline's canonical `components` map is `apps/elitea-ui/src/MainTheme.js:118-364`
— **30 keys** (unit T2 §3 confirmed the count and corrected the spec's line range).

## Wired (unit T1, then S1)

| Key | File | Scope |
|---|---|---|
| `MuiButton` | `MuiButton.ts` | All 14 variants now wired. T1 wired token colour for the six that carried the §4.1 Blocker-1 hard-coded accents (`special`, `contained`, `secondary`, `iconCounter`, `auxiliary`, `maxi`). Unit S1 (Part B) added the remaining eight (`iconLabel`, `tertiary`, `alarm`, `elitea`, `text`, `icon`, `neutral`, `positive`) plus the geometry `maxi`/`icon` need. Their `50%`-radius pill/circle shape now has a token — `theme.vars.shape.radiusPill` — added across `schema.ts`/`gen-brand-tokens.mjs`/`buildTheme.ts`/`theme.augment.d.ts` (the generated `default.pack.json`/`palette.augment.d.ts` were re-run from the generator, not hand-edited). `elitea`'s four `color` skins are separate `props: { variant, color }` entries, not a `color`-branching callback — see the file's own header comment for why. |
| `MuiChip` | `MuiChip.ts` | Canonical `root` + `outlined` slots, complete. Admin-ui's scheme-branching variant is deliberately NOT ported (T2 §3, class (b)). |
| `MuiAccordion` | `MuiAccordion.ts` | Platform-parity wave, not a baseline key (the baseline never styles `Accordion` at all). Measured directly against production (`getComputedStyle` on live `next.elitea.ai` nodes): 4px corners (`radiusSm`), no border, MUI's own `:first-of-type`/`:last-of-type` partial rounding on a stacked sibling list left unchanged. This app's ambient `theme.shape.borderRadius` is `radiusMd` (8px), so the override re-declares MUI's own two rules substituting `radiusSm` — the single-token gap, nothing more. `MuiAccordionSummary` deliberately left unwired: production shows no override on it either. |

## Owned by unit S1 — 28 keys

`MuiToggleButton`, `MuiTextField`, `MuiInput`, `MuiIconButton`, `MuiDataGrid`,
`MuiDialog`, `MuiTreeItem`, `MuiList`, `MuiMenuItem`, `MuiFormControl`,
`MuiFormHelperText`, `MuiCssBaseline`, `MuiAvatar`, `MuiPaper`, `MuiSelect`,
`MuiMenu`, `MuiTablePagination`, `MuiTab`, `MuiTabs`, `MuiAlert`, `MuiRadio`,
`MuiCheckbox`, `MuiSwitch`, `MuiDrawer`, `MuiAppBar`, `MuiBadge`, `MuiTooltip`,
`MuiAutocomplete`.

No stub files are committed for these: an empty override is indistinguishable from
a forgotten one, and `muiOverrides()` composing only real files keeps the wired set
greppable. The contract test asserts every key `muiOverrides()` returns has a render
surface in `__tests__/surfaces.tsx`, so adding a key here without widening the
hostile-pack sweep fails §4.6 check 7.

## Notes S1 needs before starting

1. **`MuiAlert` must be re-authored, not ported.** The baseline uses the CSS named
   colours `'green'`, `'red'`, `'orange'` for `filledSuccess`/`filledError`/
   `filledWarning` in *both* elitea-ui and admin-ui (T2 §3 calls it a shared defect).
   The tokens that replace them now exist: `palette.error.*`, `palette.success.*`
   (added by T1 per §4.2) and `palette.warning.*` (already in the baseline).
   `filledInfo` already uses `darkBlue` → `palette.info.main`.

2. **Sizes come from typography variants.** `theme.typography.labelSmall` (and the
   nine siblings) are declared in `src/shared/brand/theme.augment.d.ts` and built by
   `typography.ts`. `labelLarge` does not exist — it was dead in the baseline (T2 §3).
