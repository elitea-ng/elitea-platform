# Brand-hue map — unit T1 (Wave 0)

**Owner:** unit T1 (`src/shared/brand/**`). **Consumers:** the operator (§8 below is a
decision packet), unit S1 (component authoring), unit R2 (providers), unit W3 (Go pack mirror).
**Inputs:** spec §4.1–4.6, unit T2's `parity/theme-fork-triage.md`, the pinned baseline
`apps/elitea-ui@a55f36cf` (read-only). **Date:** 2026-07-27.

This file does three jobs:

1. records how spec §4.1 **Blocker 1** ("the brand hue flips between modes") was resolved —
   all fifteen hard-coded call sites, tabulated, with the token each became;
2. documents the **derivation architecture** that makes `brand.hue` a single data field;
3. carries the **human decision** (§8) the architecture defers: which hue Elitea declares.

Nothing in §8 changes what ships. The default pack states every token verbatim, so rendering
is byte-identical to the baseline (N4) whichever way the decision goes.

---

## 1. Blocker 1 — the fifteen call sites

`apps/elitea-ui/src/[fsd]/shared/ui/button/BaseBtn.jsx` carries **15** `theme.palette.mode ===`
branches. Spec §4.1 cites the ranges `52-60` and `700-726`; the full census is the table below
(the five rows at 88/103/144/149/154 are in the same file and the same class of defect —
they branch on the scheme rather than reading a token — and are included here because the
resolution must remove all of them, not ten of them).

Ten of the thirty values are raw colour literals: two accent ramps, cyan in dark and magenta
in light. That is the blocker: "the brand colour" is two hues in ~15 places, so a white-label
pack that set one colour would repaint half the UI.

Every row below became **one token id with a per-scheme value**, copied verbatim from the call
site. Rendering is unchanged; the branch is gone; the value is now white-labelable.

| # | `BaseBtn.jsx:` | dark value | light value | paints | → token id |
|---|---|---|---|---|---|
| 1 | 52 | `rgba(106, 232, 250, 0.2)` | `rgba(245, 81, 249, 0.2)` | `special` button, resting background | `background.button.special.default` |
| 2 | 53 | `primary.main` → `#6ae8fa` | `text.secondary` → `#0E131D` | `special` button, resting label | `text.button.specialDefault` |
| 3 | 56 | `rgba(106, 232, 250, 0.3)` | `rgba(245, 81, 249, 0.3)` | `special` button, hover background | `background.button.special.hover` |
| 4 | 59 | `rgba(106, 232, 250, 0.1)` | `rgba(245, 81, 249, 0.1)` | `special` button, active background | `background.button.special.pressed` |
| 5 | 60 | `primary.pressed` → `rgba(42, 189, 210, 1)` | `text.secondary` → `#0E131D` | `special` button, active label | `text.button.specialPressed` |
| 6 | 88 | `text.default` → `#A9B7C1` | `text.secondary` → `#0E131D` | `secondary` button, active label | `text.button.secondaryPressed` |
| 7 | 103 | `background.button.secondary.default` → `rgba(255, 255, 255, 0.10)` | `background.button.secondary.pressed` → `rgba(61, 68, 86, 0.2)` | `iconCounter` button, active background | `background.button.iconCounter.pressed` |
| 8 | 144 | `primary.pressed` → `rgba(42, 189, 210, 1)` | `primary.main` → `rgba(196, 40, 221, 1)` | `auxiliary` button, resting label | `text.button.auxiliaryDefault` |
| 9 | 149 | `primary.hover` → `#83EFFF` | `text.button.auxiliary` → `rgba(244, 124, 255, 1)` | `auxiliary` button, hover label | `text.button.auxiliaryHover` |
| 10 | 154 | `text.button.disabled` → `#686C76` | `secondary.main` → `#777A83` | `auxiliary` button, active label | `text.button.auxiliaryPressed` |
| 11 | 700 | `rgba(41, 184, 245, 0.2)` | `rgba(196, 40, 221, 0.2)` | `maxi` FAB, resting background | `background.button.maxi.default` |
| 12 | 701 | `primary.main` → `#6ae8fa` | `text.primary` → `#777A83` | `maxi` FAB, icon colour | `text.button.maxiDefault` |
| 13 | 718 | `rgba(41, 184, 245, 0.3)` | `rgba(196, 40, 221, 0.3)` | `maxi` FAB, hover background | `background.button.maxi.hover` |
| 14 | 722 | `rgba(41, 184, 245, 0.3)` | `rgba(196, 40, 221, 0.3)` | `maxi` FAB, focus-visible background | `background.button.maxi.hover` (same token as row 13 — the baseline duplicates the value) |
| 15 | 726 | `rgba(41, 184, 245, 0.1)` | `rgba(196, 40, 221, 0.1)` | `maxi` FAB, active background | `background.button.maxi.pressed` |

**14 new token ids** cover the 15 rows (rows 13 and 14 share one — the baseline gives
`:hover` and `:focus-visible` the same value). They are wired in `src/shared/brand/mui-overrides/MuiButton.ts`, which contains zero
raw colours and zero scheme branches — theme-gate checks 1 and 2 hold over it.

Note the two ramps use **different** cyans: `special` is built on `#6ae8fa` (the dark
`primary.main`) and `maxi` on `#29B8F5` (`skyBlue`, the dark `text.link`/`status.draft`
colour). Both survive verbatim as separate tokens; the derivation keeps them 10.4° apart because
both fall inside the brand family (§3).

---

## 2. What "resolved" means here

Blocker 1 is resolved **structurally**, not by picking a colour:

- there is now exactly one place a hue can be set — `pack.brand.hue`, a single required field;
- no component reads a scheme branch, so no component can disagree with the pack;
- the shipped look is unchanged, because the default pack states all 362 token ids per scheme.

The colour question itself is now a one-field data decision (§8) that can change later with
zero code churn.

---

## 3. Derivation architecture

`toMuiPalette(record, brand, scheme)` resolves a scheme in two steps:

1. **Stated wins.** Any token id the pack states is used verbatim.
2. **The rest is derived** from the default pack's value for that id in that scheme — the
   *reference geometry* — re-hued for `pack.brand.hue`.

### 3.1 The anchor is per scheme

```
delta(scheme) = hue(pack.brand.hue) − hue(defaultPack.schemes[scheme]['primary.main'])
```

The reference is **each scheme's own accent**, not one global hue. The baseline's two accents
are 104.2° apart (cyan 187.5°, magenta 291.7°), so a single global reference would land the two
schemes on two different hues — the blocker, re-created inside the derivation. With per-scheme
anchors, one `brand.hue` puts **both** schemes' accents on that hue. That property is asserted
in `__tests__/toMuiPalette.test.ts` ("lands BOTH schemes on the pack hue").

### 3.2 Three bands, because not every colour means the same thing

`deriveColor` (`src/shared/brand/color.ts`) classifies each colour it finds:

| band | test | transform | why |
|---|---|---|---|
| **neutral** | `saturation < 0.06` | adopt the brand hue at saturation 0.06 | A grey has no hue to rotate. Brand-tinted neutrals are what make a hue-only pack repaint surfaces, borders and dividers rather than just accents. |
| **brand family** | hue within **30°** of the scheme's anchor | rotate by the full `delta` | The accent's relatives — the link blue at 197.9°, the info blue at 208.7° next to a 187.5° anchor — must keep their exact angular relationship to it, or the ramp stops looking designed. |
| **semantic** | everything else | move toward the brand hue by at most **15°** | Harmonisation, not rotation. A red error state stays red under a green brand while picking up enough of it to belong. Rotating it would not be a repaint, it would be a loss of meaning. |

Lightness and alpha — the contrast geometry of the reference pack — are preserved exactly in
all three bands, except that derived lightness is clamped to `[3%, 97%]`. Without that clamp a
re-hued `#FFFFFF` comes back as `#FFFFFF` and a repainted pack still shows the source pack's
white; with it, a pack that genuinely wants pure white or black states that token explicitly.

Values are re-hued **inside** arbitrary CSS: gradients, box-shadows and `inset` keywords are
walked colour by colour and everything that is not a colour is passed through. `transparent`
and `none` therefore survive derivation unchanged, by design.

### 3.3 Identity

`delta ≡ 0 (mod 360)` short-circuits to the reference value byte-for-byte, neutrals and
semantic colours included: a pack that asks for a scheme's own hue is asking for nothing to
change. The preview generator asserts this property before rendering
(`scripts/gen-hue-preview.mjs:assertIdentity`).

### 3.4 What this buys

| pack | result |
|---|---|
| default (states 362 ids/scheme) | derivation fully shadowed → pixel parity with the baseline (N4) |
| tenant stating only `brand.hue` | all 362 ids derived → the whole surface repaints from one field |
| tenant stating a hue + its own semantic colours | brand repaints, `error`/`success` stay exactly as stated |

### 3.5 Known property, flagged for a later decision

Harmonisation caps semantic movement at 15°, which is deliberate but is a *default*. A tenant
that wants its error red left completely alone states three token ids. If we later decide
semantic colours should never move at all, that is a change to **one function**
(`deriveColor`), with no schema, data or component change — and it would need §4.6 check 7's
assertion (c) re-scoped, because that assertion currently requires *every* sampled colour to
differ from the default pack.

---

## 4. `palette.error` and `palette.success` — the mandatory roles

Spec §4.2 makes both mandatory top-level roles. The baseline has neither: only nested tokens
(`text.error`, `border.error`, `status.*`). Measured on the pinned baseline:

- **20** direct `palette.error.*` / `palette.success.*` reads (`grep -rnE 'palette\.(error|success)\b' src`), and
- **7** `color="error"` / `severity="error"` component consumers with a literal severity
  (`Toast.jsx:61` and `ToastProvider.jsx:71` forward a `severity` prop generically — they render
  whichever of MUI's four severities the caller passes, not specifically `error`/`success`, so
  they are excluded from this count; found by adversarial verification's independent reproduction
  of the grep below, `grep -rnE 'color=[{"]?.?(error|success)|severity=' src --include='*.jsx'`,
  minus those two forwarding sites),

i.e. **27 sites** currently render MUI's defaults (`#d32f2f`, `#2e7d32`), which are not brand
colours and are not white-labelable. Spec §4.1's "10+ consumers" understates it.

Both roles now ship as ordinary token ids in the default pack, so they are white-labelable like
everything else. Mapping and evidence (all line numbers in the pinned baseline):

| token id | dark | light | source token(s) |
|---|---|---|---|
| `error.main` | `#D71616` | `#D71616` | `dangerRed` — `darkPalette.js:45` / `lightPalette.js:13`; backs `text.error`, `status.rejected`, `icon.fill.error`, `background.button.{danger,alarm.default}` |
| `error.light` | `#E74444` | `#E74444` | `hoverRed` — `darkPalette.js:46` / `lightPalette.js:14`; backs `background.button.alarm.hover` |
| `error.dark` | `#C51111` | `#C51111` | `pressedRed` — `darkPalette.js:47` / `lightPalette.js:15`; backs `background.button.alarm.pressed` |
| `error.contrastText` | `#FFFFFF` | `#FFFFFF` | `MainTheme.js:286` — `MuiAlert.filledError { color: white }` |
| `success.main` | `#2BD48D` | `#2AB37A` | `status.published` — `darkPalette.js:541` (`green`, :69) / `lightPalette.js:542` (`green`, :36) |
| `success.dark` | `#108D22` | `#108D22` | `greenDefaultBtn` — `darkPalette.js:70` / `lightPalette.js:37`; backs `status.publishedIcon`, `background.button.positive.default` |
| `success.contrastText` | `#FFFFFF` | `#FFFFFF` | `MainTheme.js:281` — `MuiAlert.filledSuccess { color: white }` |

`success.light` is deliberately **not** stated: the baseline ramp has no green lighter than
`success.main`, and inventing one would be a design decision T1 has no evidence for. MUI's
`augmentColor` derives it from the pack's `main`, so it is still a pack-derived value and still
differs from MUI's default — asserted in check 7's assertion (d).

`border.error` (`rgba(215, 22, 22, 0.4)`) stays a separate token: it is a border alpha, not the
role colour, and folding it in would lose the alpha.

Consequence for unit S1: `MuiAlert` must be **re-authored**, not ported. Both baseline apps use
the CSS named colours `'green'`, `'red'`, `'orange'` there (T2 §3 calls it a shared defect) —
they are exactly what `elitea/no-raw-color` rejects, and the tokens that replace them now exist.

---

## 5. Reproducing the default pack

The pack is **generated**, committed, and never hand-edited:

```bash
# from apps/elitea-web
node scripts/gen-brand-tokens.mjs \
  --baseline /Users/Alexander_Kharkevich/projects/eliteaai/elitea-platform/apps/elitea-ui
# → src/shared/brand/tokens/default.pack.json      (the pack, zod-valid)
# → src/shared/brand/tokens/palette.augment.d.ts   (MUI module augmentation)

# the decision preview in this directory
node scripts/gen-hue-preview.mjs
# → parity/brand-hue-preview.html
```

`darkPalette.js` / `lightPalette.js` are evaluated as data modules (a syntactic de-module of
`export const` / `export default`, no baseline application code runs) and flattened to `a.b.c`
token ids. Everything that is *not* mechanical is one of three explicit tables in the script,
each row carrying its justification.

### 5.1 Exclusions — in the baseline, deliberately not in the pack

| dropped | why |
|---|---|
| `palette.mode` | Scheme identity, not a colour. `buildTheme` sets it per `colorScheme`; MUI's `shouldSkipGeneratingVar` skips it for CSS variables anyway. |
| `typographyVariants.labelLarge` | T2 §3, class (b): dead in the canonical app too (0 consumers outside `MainTheme.js`). Not ported to `typography.ts`. |
| `background.moderator` (+ const `red20`) | T2 §1.4/§1.16, class (b): admin-ui/Maintenance-UI only; removed from canonical in `d46cb93` (EL-5170). The script **asserts it is absent** from the canonical palettes rather than filtering it, so a regression would fail the generation. |
| `icon.fill.select` | T2 §3, class (b): phantom reference in admin-ui's `MuiRadio`/`MuiCheckbox`; exists in no palette. Also asserted absent. |

Also not ported, per T2's direction: admin-ui's `MuiChip` scheme branch, Maintenance-UI's
`eliteaTextFieldStyle` variant (an identifier that is defined nowhere) and its `sizeXs`/`sizeXl`
button sizes. Zero per-target overrides exist — every admin/Maintenance divergence T2 examined
was a stale snapshot, a trim, or a dead key.

### 5.2 Symmetry fills — 3 ids

A pack's two scheme records must carry the same key set, or a token has no reference geometry
in one scheme. The baseline has exactly three asymmetric ids. All three fills are
**pixel-neutral**: the id has no read site in the scheme being filled.

| id | filled scheme | value | evidence |
|---|---|---|---|
| `primary.hover` | light | `rgba(244, 124, 255, 1)` | `magentaHover`, already the light value of `background.button.primary.hover` and `background.tab.hover`. Sole read site is `BaseBtn.jsx:149`, inside the dark branch. |
| `text.button.auxiliary` | dark | `#83EFFF` | `primaryHover`, the value `BaseBtn.jsx:149` uses in the dark branch. Sole read site is that same line, inside the light branch. |
| `boxShadow.aiAnswer` | dark | `none` | Light-only, but read unconditionally (`ApplicationAnswer.jsx:1020`, `UserMessage.jsx:286`). In dark it currently resolves to `undefined` and the declaration is dropped; `none` is the exact CSS equivalent. |

### 5.3 Additions — 21 ids

**14** brand-accent token ids (§1, the Blocker-1 resolution) plus **7** mandatory-role token ids
(§4). Values are copied verbatim from the call site or from the nested token they replace, so no
addition changes a pixel. The script's `ADDITIONS` table is the authority and carries the
per-row citation.

### 5.4 Measured shape of the result

| metric | value |
|---|---|
| token ids per scheme | **362** (339 dark / 340 light baseline leaves → 341 after fills → 362 after additions) |
| scheme records symmetric | yes, asserted by the generator and by `__tests__/tokens.test.ts` |
| top-level palette groups | 19 (spec §4.1 ✓) |
| CSS variables emitted by `buildEliteaTheme` | 593, of which all 362 token ids resolve (asserted in `__tests__/buildTheme.test.ts`) |

---

## 6. Errata found while doing this

| where | claim | measured |
|---|---|---|
| spec §4.1 | "Palette leaf tokens: **406** dark / **406** light, structurally symmetric" | **339** dark / **340** light string leaves excluding `mode` (340/341 including it). Not symmetric: 3 ids differ (§5.2). The 19 top-level groups and the 4,182 fork LOC in the same table are both correct. |
| spec §4.1 | "10+ consumers silently get MUI defaults" for `palette.error`/`success` | **27** (20 direct palette reads + 7 `color=`/`severity=` consumers with a literal `error` value; §4 explains the 2 excluded generic-forwarding sites) |
| spec §4.2 | `docsUrl: z.string().url()` | `.url()` is deprecated in zod 4 (which the app pins at 4.4.3) and fails the D2 lint gate. Uses `z.url()` — identical schema, identical output type, so unit W3's `optionalURL` mirror stays valid. **This is the only deviation from the §4.2 schema text, and it is spelling, not semantics.** |
| baseline `MainTheme.js:21` | `headingLarge.fontStyle: 'semibold'` | Not a valid CSS `font-style` value; it computes to `normal`. The port emits `normal`, which is what the baseline actually renders. |
| baseline `BaseBtn.jsx` | spec §4.1 cites 15 call sites at `52-60,700-726` | 15 is right; the ranges cover 10 of them. Full census in §1. |

No **required** field had to be added to the §4.2 schema, so there is **no Go-side delta** for
unit W3. The derivation needs `brand.hue` and nothing else.

---

## 7. Typography, for the record

The §4.2 pack schema carries `baseSize` and `scale` but no variant table, so the ten live
variants are a **modular ladder**: `sizePx(step) = 2 · round(baseSize · scale^step / 2)`.
Rounding to *even* pixels is what makes the ladder reproduce the baseline exactly at (14, 1.2):

| step | raw px | ladder px | baseline rem | variants |
|---|---|---|---|---|
| +2 | 20.16 | 20 | `1.25rem` | headingLarge |
| +1 | 16.80 | 16 | `1rem` | headingMedium |
| 0 | 14.00 | 14 | `0.875rem` | headingSmall, labelMedium, bodyMedium |
| −1 | 11.67 | 12 | `0.75rem` | labelSmall, bodySmall, bodySmall2, subtitle |
| −2 | 9.72 | 10 | `0.625rem` | labelTiny |

The naive `round()` puts step +1 at 17px and misses `1rem` entirely. Line height and letter
spacing are stored as baseline pixel values and scaled by the variant's own size ratio, so the
default pack emits the baseline strings byte-for-byte (ratio exactly 1) while a pack with a
different `baseSize` keeps its leading proportional. Weight, style and text-transform are
design-system constants — the schema has no field for them and adding a **required** field
would break W3's mirror.

---

## 8. HUMAN DECISION — which hue does Elitea declare?

> **DECIDED — cyan, `#6ae8fa`, the recommendation in §8.5 as generated.** Closed by
> ADR-0024 (runtime branding, elitea-docs #25; this note is its WP0). `default.pack.json`
> keeps `brand.hue = "#6ae8fa"` and every stated token, so nothing shipped changed (N4 holds).
> What the value now governs, as built:
>
> 1. **A deployment hue derives everything.** A stored `brand_hue` that differs from the
>    product default's makes the server drop the default's stated scheme tokens
>    (`services/elitea-main/internal/api/v2/branding/resolver.go`), so the served pack
>    states only the hue and the client derives all 362 ids per scheme from it. The ramps in
>    §8.3 are exactly what such a deployment renders; the "cyan-unified" column is the one
>    a hue-only pack that restates `#6ae8fa` gets.
> 2. **Hand-tuned tokens ride in `scheme_tokens`** (ADR-0024 decision 9, the branding
>    package): a pack that wants more than derivation states tokens there, and stated tokens
>    win over derivation exactly as §3 describes.
> 3. **The admin Branding page starts here.** Its swatch strip and WCAG warning are computed
>    by the same `resolveScheme` from the draft hue, with this value as the initial state.
>
> The magenta alternative remains one line plus a regeneration, as §8.5 says; it is simply
> no longer open. The rest of this section is kept as the record of how the call was made.

> **This section was for the operator. Unit T1 did not make this call.**

### 8.1 What is actually being decided

`pack.brand.hue` in `src/shared/brand/tokens/default.pack.json`. One string. Today it is
`#6ae8fa` (the dark accent), chosen as a placeholder so the identity property holds for the
dark scheme.

**Changing it changes nothing about what ships.** The default pack states all 362 ids per
scheme, so derivation is fully shadowed and the app renders byte-identically either way. What
the value *does* decide:

1. what a tenant pack that says "use the Elitea hue" gets;
2. which scheme is byte-identical and which is derived, for such a pack;
3. the admin brand editor's starting point (§4.3 channel E);
4. the value any token added to the pack later inherits if it is stated in only one scheme;
5. what we tell people the brand colour is.

### 8.2 The candidates

| | Cyan family | Magenta family |
|---|---|---|
| value | `#6ae8fa` | `rgba(196, 40, 221, 1)` |
| hue | 187.5° | 291.7° |
| where it lives today | dark `primary.main`, `background.tab*`, `text.createButton`, `split.text.default`, the `special` button ramp | light `primary.main`, `background.tab*`, `background.button.primary.*`, `border.userMessageEditor`, `icon.fill.active`, the `maxi` FAB ramp |
| declaring it leaves **unchanged** | the dark scheme | the light scheme |
| declaring it **repaints** | the light scheme | the dark scheme |

### 8.3 Derived ramps (generated, not hand-picked)

Below is what a pack that states **only** `brand.hue` renders. Full side-by-side with rendered
buttons, chips, links and 20-swatch grids per scheme:
**`parity/brand-hue-preview.html`** (self-contained, open it directly).

| token | scheme | status quo | cyan-unified | magenta-unified |
|---|---|---|---|---|
| `primary.main` | dark | `#6ae8fa` | `#6ae8fa` | `#e66afa` |
| `primary.main` | light | `rgba(196, 40, 221, 1)` | `#28c6dd` | `rgba(196, 40, 221, 1)` |
| `background.button.primary.default` | dark | `#6ae8fa` | `#6ae8fa` | `#e66afa` |
| `background.button.primary.default` | light | `rgba(196, 40, 221, 1)` | `#28c6dd` | `rgba(196, 40, 221, 1)` |
| `background.button.special.default` | dark | `rgba(106, 232, 250, 0.2)` | `rgba(106, 232, 250, 0.2)` | `#e66afa33` |
| `background.button.special.default` | light | `rgba(245, 81, 249, 0.2)` | `#51d1f933` | `rgba(245, 81, 249, 0.2)` |
| `background.button.maxi.default` | dark | `rgba(41, 184, 245, 0.2)` | `rgba(41, 184, 245, 0.2)` | `#f529ee33` |
| `background.button.maxi.default` | light | `rgba(196, 40, 221, 0.2)` | `#28c6dd33` | `rgba(196, 40, 221, 0.2)` |
| `background.tab.active` | dark | `#6ae8fa` | `#6ae8fa` | `#e66afa` |
| `background.tab.active` | light | `rgba(196, 40, 221, 1)` | `#28c6dd` | `rgba(196, 40, 221, 1)` |
| `text.link` | dark | `rgba(41, 184, 245, 1)` | `rgba(41, 184, 245, 1)` | `#f529ee` |
| `text.link` | light | `#006DD1` | `#00a1d1` | `#006DD1` |
| `background.default` | dark | `#0E131D` | `#0E131D` | `#0e0f1d` |
| `background.default` | light | `rgba(248, 252, 255, 1)` | `#f0fcff` | `rgba(248, 252, 255, 1)` |
| `text.primary` | dark | `#A9B7C1` | `#A9B7C1` | `#c1a9bd` |
| `text.primary` | light | `#777A83` | `#768384` | `#777A83` |
| `status.published` | dark | `#2BD48D` | `#2BD48D` | `#2bd4b7` |
| `status.published` | light | `#2AB37A` | `#2ab39c` | `#2AB37A` |
| `error.main` | dark | `#D71616` | `#D71616` | `#d71646` |
| `error.main` | light | `#D71616` | `#d71646` | `#D71616` |

Read the semantic rows as the harmonisation band working: `status.published` stays green and
`error.main` stays red in every column — they shift, they do not change meaning. `text.link`
in the **dark/magenta** cell (`#f529ee`) is the one place the family arc bites: at a 187°
anchor the link blue (197.9°) is inside the brand family, so declaring magenta drags it with the
accent. That is correct behaviour and it is also the single most visible cost of the magenta
option.

### 8.4 Design-intent evidence (weak — one token)

A parallel scout attempted to extract design tokens from the team's Figma file
(`5vWxC85QBhqbzPU30RP7LH`). The attempt is **permanently blocked for this session**: the
account's Figma seat is *View* tier, capped at 6 MCP tool calls per month, and the quota is
exhausted. The file itself turned out to hold a single page with one "Cover" frame (the AlitA
logomark plus a "Prompt Library" text layer) — **no product screens**. `search_design_system`
returned empty (no published library), and the only endpoint that could dump the full variable
inventory with light/dark modes (`/v1/files/:key/variables/local`) is Enterprise-plan-only.
**Figma cannot arbitrate this question on the current plan by any route.**

One data point was recovered before the wall, and it points weakly toward cyan:

- Figma variable `DT/gray_50 (secondary bgr)` = `#181F2A`.
- That value **and its name** match `darkPalette.js` (`const gray50 = '#181F2A'`) exactly, in
  every shipped copy.
- The `gray_00…gray_60` navy ramp exists **only** in `darkPalette.js`; `lightPalette.js`
  borrows just `gray30`/`gray60` and defines its own `magentaDefault`.

Inference, stated as weak evidence and not proof: the Figma `DT/` collection is dark-first and
is the direct ancestor of the shipped **dark** palette, and there is no design provenance in
that file for the light-mode magenta. One token is not a verdict, and no accent or primary
token was reachable. **This does not override N4**: whatever is decided, the default pack keeps
the shipped colours verbatim.

### 8.5 Recommendation — cyan (`#6ae8fa`)

Five reasons, in decreasing weight:

1. **The dark scheme is the product's default and its majority surface.** The baseline boots
   dark (`slices/settings.js:76`: `localStorage.getItem('mode') || 'dark'`), so `:root` carries
   the dark variables and most users see cyan. Declaring the hue that most sessions already
   render minimises the gap between "the Elitea brand colour" and "the colour Elitea looks
   like".
2. **It is the cheaper derivation.** Declaring cyan leaves the dark scheme byte-identical for a
   hue-only pack and derives light. Declaring magenta derives dark — the scheme with more
   surface area, more gradients (`sideBar`, `agentModal`, `welcome`, `resourceCard`,
   `interactiveTourPrompt`) and the one users boot into.
3. **The light scheme's blues survive better than the dark scheme's.** In the cyan-unified
   light column the link (`#006DD1` → `#00a1d1`), tips and info blues stay blue, because at a
   291° anchor they sit outside the brand family and only harmonise. In the magenta-unified
   dark column they are *inside* the 187.5° family and rotate to magenta (`text.link` →
   `#f529ee`), which reads as a loss of the "link is blue" convention.
4. **The one piece of design provenance we could reach is dark-first** (§8.4) — weak, but it
   does not point the other way.
5. **The cyan family is the more distinctive of the two.** Cyan `#6ae8fa` at 187.5° is unusual
   as a product accent; magenta/purple at 291.7° is the most crowded hue in developer tooling.

Arguments the other way, stated fairly: the marketing site and any printed material may already
use the magenta; light mode is what screenshots and documentation usually show; and magenta is
the hue that appears in the *light* `primary.main`, which is the token a naive reader of the
palette would call "the primary colour".

**If the operator picks magenta instead**, the change is one line in the generator's
`PACK_META.brand.hue` plus a regeneration — no code, no tests, no schema, no Go change. Nothing
in this document's §1–§7 depends on the outcome.

---

## 9. What lands where

| artefact | owner | note |
|---|---|---|
| `src/shared/brand/tokens/default.pack.json` | T1 (generated) | 362 ids/scheme; regenerate, never hand-edit |
| `src/shared/brand/tokens/palette.augment.d.ts` | T1 (generated) | typed `theme.vars.palette.*` for every id |
| `src/shared/brand/mui-overrides/` | T1 structure, **S1 content** | 2 of 30 keys wired; see `OWNERSHIP.md` for the remaining 28 and the two blockers S1 will hit |
| `InitColorSchemeScript` wiring | **R2** | exact snippet and the matching-props caveat are in `src/shared/brand/constants.ts` (`INIT_COLOR_SCHEME_PROPS`); F1 owns `index.html` and T1 did not touch it |
| `brand.hue` value | **operator** | §8 |

---

## 10. A11Y follow-up — the `#D71616` family, dark scheme (2026-07-27)

Unit S1-H's `SecretField` `ErrorState` story surfaced a real WCAG AA failure
this document's own §4 table had already, unknowingly, recorded the cause of:
`error.main` (and everything else sourced from the baseline's `dangerRed`
const) is `#D71616` in **both** schemes, byte-for-byte — confirmed by grep to
be exactly 6 ids: `error.main`, `icon.fill.error`, `text.error`,
`status.rejected`, `background.button.alarm.default`,
`background.button.danger`. This is not a porting gap — `apps/elitea-ui`'s
own `darkPalette.js:45` and `lightPalette.js:13` both hard-code the identical
literal, so a verbatim mechanical port (§5) reproduces a defect the baseline
always shipped with. Unit T2's triage (`parity/theme-fork-triage.md`) never
flagged it because that document's axis is elitea-ui-vs-admin-ui-vs-Maintenance
fork divergence, not elitea-ui's own dark/light contrast.

Measured against the tokens' REAL dark-scheme rendering surfaces (WCAG 2.1
relative-luminance formula, not axe's approximation):

| id | real dark consumer | bar | ratio before | ratio after | changed? |
|---|---|---|---|---|---|
| `icon.fill.error` | `MuiTextField.ts`'s `.Mui-error` `FormHelperText` text; `MuiSelect.ts` border | 4.5:1 (text) | 3.56:1 (`background.default`) / 3.17:1 (`background.card.default`) | 5.05:1 / 4.50:1 | **yes -> `#ED4F4F`** |
| `text.error` | none yet (pre-emptive) | 4.5:1 (text) | 3.56:1 / 3.17:1 | 5.05:1 / 4.50:1 | **yes -> `#ED4F4F`** |
| `status.rejected` | text/icon colour at >10 baseline call sites (`AnalyticsTools.jsx` et al.) | 4.5:1 (text) | 3.56:1 / 3.17:1 | 5.05:1 / 4.50:1 | **yes -> `#ED4F4F`** |
| `background.button.alarm.default` | fill under `text.button.primary` label (`MuiButton.ts` `alarm`/`elitea+alarm`) | 4.5:1 (text-on-fill) | 3.56:1 | 5.16:1 | **yes -> `#ED4F4F`** |
| `error.main` | fill under `error.contrastText` (`MuiAlert.ts` filled) | 4.5:1 (text-on-fill) | 5.23:1 | 5.23:1 (unchanged) | no — already passing |
| `background.button.danger` | icon fill only (`BannerMessage.tsx`), on `background.errorBkg` over `background.default` | 3:1 (icon, SC 1.4.11) | 3.45:1 | 3.45:1 (unchanged) | no — already passing |

`error.main` cannot be lightened to also clear the text bar without dropping
`error.contrastText`'s own 4.5:1 below AA (verified by exhaustive search over
the same hue/saturation ramp — no lightness value clears both). It is a
`PaletteColor` fill role paired with `contrastText`, not a text role;
`text.error` is the id for text contexts and is the one that moved.

All four moved ids keep the baseline's exact hue (0deg) and saturation
(~0.814), lightness raised from 0.465 to 0.62 — the same kind of same-hue
lightness move `color.ts`'s `deriveColor` makes for its NEUTRAL band (§3.2),
applied by hand because this is a fixed-purpose contrast correction, not a
`brand.hue` re-theme. Light scheme is untouched: `#D71616` already clears AA
there (5.07:1 against `background.default` `rgba(248, 252, 255, 1)`).

Mechanism: `scripts/gen-brand-tokens.mjs` gained a fourth table,
`A11Y_OVERRIDES` (after EXCLUSIONS/SYMMETRY_FILLS/ADDITIONS), applied inside
`applyTables` after `applyAdditions`. It asserts the baseline literal it was
measured against is still present before overwriting, so a future
`--baseline` re-point that changes `dangerRed` fails loudly instead of
silently reintroducing or masking the defect. `default.pack.json` was
regenerated through this table, not hand-edited — diff is exactly the four
values above (plus one unrelated pre-existing `shape.radiusPill` drift
between `PACK_META` and the last-committed pack, incidentally caught by the
same regeneration).

**Fast-follow, landed (2026-07-27):** `background.button.alarm.pressed`
(`#C51111`, a different literal from the `#D71616` family above, so a
separate `A11Y_OVERRIDES` row) measured 3.07:1 against `text.button.primary`
for the same `MuiButton.ts` `alarm`/`elitea+alarm` `:active` label. `.hover`
(`#E74444`) was re-checked and already passes at 4.70:1 (l=0.5863),
unchanged. Fixing `.default` above (l=0.465 -> 0.6196) had made it lighter
than `.hover` — an inverted default/hover ordering versus the baseline's
original ramp, accepted as-is since re-deriving `.hover` was out of scope for
that pass. For `.pressed`, an exhaustive lightness scan (same hue 0deg,
matching saturation ~0.8145) over the codebase's own `color.ts`
`hslaToRgba`/`rgbaToHsla` — not hand math — found the valid band for
"passes 4.5:1 AND stays darker than `.hover`" is narrow: HSL lightness
(0.566, 0.586). Chose `l=0.575` (`#EB3A3A`, 4.60:1) — comfortable margin
above the 4.5:1 floor without crowding `.hover`'s own lightness. Final ramp:
`.default` (l=0.6196, 5.16:1) > `.hover` (l=0.5863, 4.70:1) > `.pressed`
(l=0.575, 4.60:1) — monotonically darker through the interaction sequence,
not inverted despite `.default`'s earlier fix.
