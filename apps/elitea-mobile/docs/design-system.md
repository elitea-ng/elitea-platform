# Agent Zefir design system v1

Status: proposal for the mobile redesign (agent-zefir, Expo SDK 58 / RN 0.88).
Companions: `design-tokens.json` (values; the source for code), `screens.md` (screen layouts).
Inputs: `reference-ui.jpg` (pastel glass assistant UI), `mascot-zefirca-original.jpeg`
("Agent Zefirca"), agent-zefir `origin/main` @ 654061a (`src/theme/*`,
`src/constants/theme.ts`, `src/constants/use-palette.ts`, `app.config.ts`,
`docs/design.md`), elitea-platform `origin/main` brand pack
(`apps/elitea-web/src/shared/brand/schema.ts`, `tokens/default.pack.json`,
`services/elitea-main/internal/api/v2/branding/`).

---

## 0. What is wrong today (the starting point)

Read from `origin/main`. These are the issues the redesign has to fix.

| Area | Current state | Guideline problem |
|---|---|---|
| Navigation | One `Stack`. The Conversations header holds a ProjectChip, the bell and the text links "New" and "Settings" (`src/app/(app)/_layout.tsx`). | HIG and M3 both expect top-level destinations in a tab bar or navigation bar. Text links in a nav bar are inconsistent with platform chrome, and there are four header items on a phone. |
| Palette | 8 tokens. Fallback accent `#208AEF` and splash `#208AEF` are the Expo template blue. Pure `#000`/`#fff` grounds. | No product identity. Pure black and white with blue reads as a template. Dark mode has no elevation steps. |
| Icons | `icon.png`, `assets/expo.icon` and the Android adaptive icon (`#E6F4FE`) are template placeholders (TODOs in `app.config.ts`). | Store review: the icon must be final. The iOS 26 layered icon and the Android 13 themed icon are not designed. |
| Type | Ad-hoc `fontSize: 16` and weights in the components. | No Dynamic Type or font-scale ramp, and no text-style mapping. |
| Branding | `usePalette` spreads the pack's `background/text/backgroundElement/border/accent/danger` over the fallback. When the server serves Elitea's product default (`productDefault: true`), Elitea's web colours (`#C428DD` light, `#6AE8FA` dark) still override the app's palette. Tokens that fail contrast are dropped, not corrected. | Web-tuned neutrals replace the app's structure. The app looks like a different product on every server, and the proofs below cannot hold for arbitrary neutrals. |
| Avatars | A letter on `palette.border` (28 px) for everyone. | Agents have no identity. Nothing tells an agent apart from a person. |

---

## 1. Visual language

**Personality:** friendly, soft and calm, but competent. The reference supplies the
*structure*: a big greeting, glass cards, circular arrow buttons, a chip row and a
floating composer. The mascot supplies the *palette*: marshmallow pink, cream, coat blue,
mint pedestal and belt orange. Everything sits on a slow, very light pastel gradient. The
saturated colours are deep "ink" versions of the mascot hues, so text and controls pass
WCAG AA.

### 1.1 Palette primitives (from the mascot)

| Family | Source on the mascot | Light use | Dark use |
|---|---|---|---|
| Marshmallow (pink) | head swirl, tie (sampled ~`#F2B4BE`) | gradient stop `#FCEBF2`, accent `#B4306A` (600), soft `#FCE4EE` | accent `#FF8CB8`, soft `#3D1E2D` |
| Cream | face, shirt | background `#FBF7F9` | n/a (night neutrals) |
| Coat (blue) | coat (sampled `#BDD9E3`→`#9FB4C0`) | secondary/focus `#2D6894`, soft `#DDEBF5` | `#8DC3EA` |
| Mint | pedestal (sampled `#CBE9DA`) | gradient end `#E8F5EF`, success `#1D7A4C` | `#5CD49C` |
| Belt (orange) | belt, antenna light (sampled `#E4A176`) | warning `#8F5300`, highlight sparkle only | `#F3B54A` |
| Ink / Night | the plum-tinted neutrals (not pure grey) | text `#1C1622`… | background `#141118`… |

Pastels (`marshmallow.200-400`, `coat.300`, `mint.200`, `belt.300`) are **decorative
only**: gradients, illustration and the orb. They never carry text and never mark state.
Their contrast is about 1.3–2.5:1.

### 1.2 Gradient ground

- One full-screen `LinearGradient` per root screen, at 160°, with three stops:
  - light: pink mist `#FCEBF2`, lavender `#F2EEFB`, mint mist `#E8F5EF`
  - dark: `#1F1520`, `#16151F`, `#111B19`
- The gradient sits behind the scroll content and does not scroll (no parallax).
- Chat and long reading surfaces use the flat `background` instead, so the gradient does
  not sit behind dense text. The gradient appears on Home, the connect, sign-in and voice
  screens, and empty states.
- When `Reduce Transparency` (iOS) is on, or high-contrast text (Android) is on, use the
  flat `background`.

### 1.3 Glass and surfaces

| Token | Light | Dark | Use |
|---|---|---|---|
| `surface` | `#FFFFFF` | `#1D1A22` | list rows, bubbles, settings groups |
| `elevated` | `#FFFFFF` + e2 | `#26222C` | sheets, menus, cards over cards |
| `surfaceGlass` | white α .72, blur 24, 1 px border α .9 | `#2A2631` α .72, blur 24, border white α .08 | composer, action cards, chips on gradient, header buttons |
| `surfaceSunken` | `#F5EFF3` | `#100E13` | text field interiors, code blocks |

Platform rendering of glass:

- **iOS 26+:** use the system Liquid Glass material (`expo-glass-effect` `GlassView`) for
  the composer, floating header buttons and the tab bar (the native tabs provide this
  themselves).
- **iOS < 26:** use `expo-blur` `BlurView` (`systemUltraThinMaterial`) under a tint layer
  of the `surfaceGlass` colour.
- **Android:** no real-time blur, because it is expensive and inconsistent. Use the
  `surfaceGlass.opaqueFallback` colour at α .92 with an e1 shadow. This is M3's tonal
  surface.
- **Reduce Transparency, or a low-power device:** use `opaqueFallback` at α 1.
- **Contrast proofs** for glass are computed composited over every gradient stop and the
  flat background, so the proofs also hold for the opaque fallback.

### 1.4 Spacing (4/8 grid)

Steps are 0, 2, 4, 8, 12, 16, 20, 24, 32, 40, 48, 64, with these aliases:

- screen gutter 20 (16 when the width is under 360 pt or the content size is an
  accessibility size)
- card padding 16 and card gap 12
- section gap 28
- list row vertical padding 12
- composer inset 12 from the safe area

Layouts follow the safe areas (`react-native-safe-area-context`). Content max width is
640, centred on iPad and tablets.

### 1.5 Radii

- chip and button: pill
- card 22, and action card 28 (the reference uses large soft corners)
- bubble 22, with a 6 corner on the speaker side
- composer 28, list row 16, sheet top 32

On iOS, set `borderCurve: 'continuous'` on every rounded view (squircle, which matches
HIG). Android uses plain radii, which are within one step of the M3 shape scale.

### 1.6 Elevation

There are three levels (`e1` cards, `e2` floating composer/FAB, `e3`
sheets/menus).

- **Light:** shadows are tinted plum (`#6E1A3B`) at very low opacity. This keeps them
  "soft" like the reference. Grey shadows look dirty on pink.
- **Dark:** elevation shows as lighter surfaces (`surface` → `elevated` →
  `surfaceGlass`) plus a 1 px top highlight `rgba(255,255,255,.06)`. This follows M3
  tonal elevation and HIG dark-mode guidance.
- **Android:** set `elevation` as listed in `design-tokens.json`. On a translucent view, set it
  only on an opaque wrapper. Otherwise Android draws the shadow through the glass.

### 1.7 Typography

Use the platform system fonts: SF Pro on iOS and Roboto on Android. The display role on
iOS uses **SF Pro Rounded** (`fontFamily: 'ui-rounded'`, available on iOS), which matches
the mascot's softness. Android keeps Roboto at weight 500 for display, as in M3.

| Role | iOS (text style, pt) | Android (M3, sp) | Used for |
|---|---|---|---|
| display | largeTitle 34/41 bold, rounded | displaySmall 36/44 | Home greeting |
| title1 | 28/34 bold | headlineMedium 28/36 | Voice screen title, connect hero |
| title2 | 22/28 bold | titleLarge 22/28 | screen titles (when not large-title nav) |
| title3 | 20/25 semibold | 18/24 medium | sheet titles |
| headline | 17/22 semibold | titleMedium 16/24 | card titles, row titles, agent names |
| body | 17/22 | bodyLarge 16/24 | message text, inputs |
| callout | 16/21 | bodyMedium 14/20 | card descriptions, tool-step text |
| subheadline | 15/20 | labelLarge 14/20 medium | chip labels, buttons |
| footnote | 13/18 | bodySmall 12/16 | row previews, timestamps |
| caption | 12/16 | labelMedium 12/16 | metadata, section "See all" |
| label | 11/13 semibold | labelSmall 11/16 | tab labels, badges |
| code | Menlo 15/20 | monospace 14/20 | code blocks, tool arguments |

**Scaling rules (Dynamic Type and Android font scale):**

- RN scales `fontSize` with the OS setting by default. Never pass
  `allowFontScaling={false}` to content.
- Cap only chrome with `maxFontSizeMultiplier`. The per-role values are in `design-tokens.json`;
  for example, tab labels are capped at 1.5 and badges at 1.5.
- Store font sizes in tokens. Compute line heights as `size × ratio`, so they scale with
  the size.
- **Accessibility layout switch** at `PixelRatio.getFontScale() ≥ 1.35`. When it applies:
  - the action-card grid becomes a single-column list
  - the chip row wraps instead of scrolling
  - the bubble action row moves into a "…" menu
  - the greeting drops to title1
  - the tab bar hides labels on Android only, because iOS native tabs handle this
    themselves.
- Never truncate message text. Titles truncate to 1 line at standard sizes and 2 lines
  at accessibility sizes.
- **Workspace font:** a brand pack `.ttf` or `.otf` face (already vetted by `mapFonts`)
  may apply to **display, title1–3 and headline only**. Body, metadata and code always
  use the system font, which keeps Dynamic Type metrics and legibility.
  - **Change from today:** `fontFamily` is currently applied wherever the alias is used.
    Restrict it to these roles.
  - The workspace host and URL are always set in the system font (existing rule).

### 1.8 Iconography

- **iOS:** use SF Symbols through `expo-symbols` `SymbolView`. Weight matches the
  adjacent text (regular next to body, semibold next to headline). Scale is `medium`.
  Filled variants mark selected states only.
- **Android:** use Material Symbols *Rounded* (rounded matches the brand), weight 400,
  fill 0. Fill 1 marks the selected tab.
  - Render them through expo-symbols' Android Material support where the installed
    version provides it. Otherwise use a bundled Material Symbols Rounded font subset.
  - Check this against the installed `expo-symbols` version before building.
- The **icon map** (`design-tokens.json → icons.map`) pairs every app icon as
  `[sfSymbol, materialSymbol]`. Components take the semantic name only.
- **Sizes:** 20 inline, 22 in icon buttons, 24 in the tab bar. Icon-only controls always
  have an `accessibilityLabel`.
- **No custom icon set.** The mascot and the hologram gear motif are illustrations, not
  icons.

### 1.9 Motion

- **Durations:** instant 100, fast 180, base 260, slow 400.
- **Curves:**
  - M3 "standard" `(0.2,0,0,1)` for state changes
  - "emphasized decelerate" for things entering, "emphasized accelerate" for exits
  - springs (Reanimated 4, already a dependency) for user-driven motion: default
    d20/s220, bouncy for the mascot and orb, snappy for swipe snap-back.
- **Signature moments:**
  - the greeting fades up (8 px) on Home load
  - the action cards stagger in at 40 ms each
  - the composer grows with spring height
  - the send button morphs mic ↔ send ↔ stop
  - the orb breathes (2.4 s loop) and scales by mic level
  - a streaming caret pulses
  - the mascot does a gentle 4° tilt on empty states (once, not looping).
- **Reduced motion** (`useReducedMotion()` from Reanimated, and
  `AccessibilityInfo.isReduceMotionEnabled`):
  - springs, translations and scales become 150 ms opacity cross-fades
  - the orb shows a static ring whose thickness steps with the audio level (no
    breathing)
  - the caret is static and the stagger is off.
- **Platform transitions:** do not override them. Use the iOS push and Android predictive
  back (see the note in `screens.md`). Sheets use native presentation
  (`presentation: 'formSheet'` with detents on iOS, M3 bottom sheet on Android).

### 1.10 Haptics (`expo-haptics`, new dependency)

| Event | Haptic |
|---|---|
| send message | impact light |
| voice start / stop | impact medium / light |
| copy | notification success |
| like / dislike, chip and tab change | selection |
| swipe action passes its threshold | impact rigid (Android: `performAndroidHapticsAsync` Confirm when available) |
| approve / reject a tool | success / warning |
| error | notification error |

Haptics fire only for user-initiated actions, never for streaming tokens or incoming
notifications. The OS setting mutes them, so no in-app toggle is needed.

---

## 2. Colour tokens (light and dark) with WCAG proofs

The values are in `design-tokens.json → color.light / color.dark`. The semantic set:

`background`, `backgroundGradient`, `surface`, `surfaceSunken`, `surfaceGlass`,
`elevated`, `textPrimary`, `textSecondary`, `textTertiary`, `separator` (decorative
hairline), `separatorStrong` (input borders, 3:1), `accent`, `accentPressed`, `accentOn`,
`accentSoft`, `accentSoftOn`, `secondary`, `secondarySoft`, `secondarySoftOn`,
`success`, `successSoft`, `warning`, `warningSoft`, `danger`, `dangerOn`, `dangerSoft`,
`bubbleUser`, `bubbleUserText`, `bubbleAgent`, `bubbleAgentText`, `bubbleAgentBorder`,
`chip`, `chipText`, `chipSelected`, `chipSelectedText`, `badge`, `badgeText`, `focus`,
`scrim`, `orb` (decorative), `skeleton`.

### 2.1 Proofs

The ratios below were computed with the WCAG 2.x formula in `src/theme/color.ts`, using a
script over `design-tokens.json`. The pass bars are:

- AA normal text 4.5
- AA large text and non-text UI 3.0

**Every text pair clears 4.5, including tertiary.** Metadata is small, so there is no
"large text" exemption. Glass is proven against its **worst** composite over every
gradient stop and the flat background. Gradient rows use the worst stop.

| Foreground on background | Light | Dark | Bar |
|---|---|---|---|
| textPrimary / background | 16.66 | 16.89 | 4.5 |
| textPrimary / gradient (worst stop) | 15.42 | 15.88 | 4.5 |
| textPrimary / surface | 17.69 | 15.51 | 4.5 |
| textPrimary / surfaceGlass (worst) | 16.98 | 14.10 | 4.5 |
| textPrimary / elevated | 17.69 | 14.08 | 4.5 |
| textPrimary / surfaceSunken | 15.60 | 17.34 | 4.5 |
| textSecondary / background | 7.02 | 9.32 | 4.5 |
| textSecondary / gradient (worst) | 6.50 | 8.77 | 4.5 |
| textSecondary / surface | 7.45 | 8.56 | 4.5 |
| textSecondary / surfaceGlass (worst) | 7.15 | 7.78 | 4.5 |
| textSecondary / elevated | 7.45 | 7.77 | 4.5 |
| textTertiary / background | 5.17 | 6.33 | 4.5 |
| textTertiary / gradient (worst) | 4.79 | 5.95 | 4.5 |
| textTertiary / surface | 5.49 | 5.81 | 4.5 |
| textTertiary / surfaceGlass (worst) | 5.27 | 5.28 | 4.5 |
| textTertiary / elevated | 5.49 | 5.28 | 4.5 |
| accent / background (as link text) | 5.54 | 8.63 | 4.5 |
| accent / gradient (worst) | 5.13 | 8.12 | 4.5 |
| accent / surface | 5.88 | 7.93 | 4.5 |
| accent / surfaceGlass (worst) | 5.64 | 7.21 | 4.5 |
| accent / elevated | 5.88 | 7.19 | 4.5 |
| accentOn / accent (button label) | 5.88 | 7.80 | 4.5 |
| accentSoftOn / accentSoft | 6.70 | 9.83 | 4.5 |
| secondary / surface | 5.97 | 9.10 | 4.5 |
| secondary / gradient (worst) | 5.20 | 9.31 | 4.5 |
| secondarySoftOn / secondarySoft | 7.69 | 10.10 | 4.5 |
| success / surface | 5.33 | 9.28 | 4.5 |
| warning / surface | 6.17 | 9.42 | 4.5 |
| danger / surface | 5.90 | 7.38 | 4.5 |
| danger / gradient (worst) | 5.15 | 7.56 | 4.5 |
| dangerOn / danger | 5.90 | 7.45 | 4.5 |
| bubbleUserText / bubbleUser | 5.88 | 11.76 | 4.5 |
| bubbleAgentText / bubbleAgent | 17.69 | 14.66 | 4.5 |
| chipText / chip | 17.69 | 14.08 | 4.5 |
| chipText / surfaceGlass (worst) | 16.98 | 14.10 | 4.5 |
| chipSelectedText / chipSelected | 6.70 | 9.83 | 4.5 |
| badgeText / badge | 5.90 | 4.91 | 4.5 |
| separatorStrong / surface (input border) | 3.55 | 3.60 | 3.0 |
| separatorStrong / background | 3.34 | 3.92 | 3.0 |
| separatorStrong / surfaceSunken (selected segment edge) | 3.13 | 4.02 | 3.0 |
| focus / background (focus ring) | 5.62 | 9.91 | 3.0 |
| focus / gradient (worst) | 5.20 | 9.31 | 3.0 |
| focus / surface | 5.97 | 9.10 | 3.0 |

**Not proven, by design:**

- `separator` (`#E6DDE3`, `#332E3A`) is a decorative hairline. Grouping is always also
  shown by spacing or a surface change (WCAG 1.4.11 does not apply).
- The `orb` and the gradients are decorative.
- Never use the pastel primitives for text or state.

**Re-proving:** any token change re-runs the proof script. It should become a unit test
next to `src/theme/color.test.ts` that iterates `design-tokens.json → proofs` over both schemes.

### 2.2 State colours

- **Pressed:** `accentPressed` for filled controls. Elsewhere, an overlay of
  `textPrimary` at α .08 (light) or white at α .10 (dark). On Android this is
  additionally the ripple (`android_ripple`), with the colour set to the same overlay.
- **Disabled:** content at α .38 and container at α .12 (M3). Disabled controls are
  exempt from contrast, but they must also be marked
  `accessibilityState={{disabled:true}}`.
- **Focus** (keyboard, switch control, Android D-pad): a 2 pt `focus` ring offset by 2 pt.

---

## 3. Workspace branding rules

The app is always **Agent Zefir**. A workspace *accents* it. It never re-skins it.

### 3.1 What a brand pack overrides

| Overridden from the pack | Pack source (per scheme) | Notes |
|---|---|---|
| `accent` (+ `accentPressed`) | `schemes.<s>["primary.main"]`, else `brand.hue` | contrast-corrected (3.3) |
| `accentOn` | `schemes.<s>["text.button.primary"]`, else `brand.onBrand`, else auto black/white | must reach 4.5 on the corrected accent |
| `accentSoft` / `accentSoftOn` | derived from the corrected accent hue (OKLCH) | soft L .94/.27, C ≤ .045 |
| `bubbleUser` / `bubbleUserText` | light: the corrected accent; dark: OKLCH(L .33, C ≤ .10, brand hue) | text auto-picked, ≥ 4.5 |
| `chipSelected` / `chipSelectedText` | = accentSoft / accentSoftOn | |
| gradient tint | stop 0 of `backgroundGradient` → OKLCH(L .95 light / .19 dark, C .03, brand hue) | dropped if `textTertiary` falls below 4.5 on it |
| orb tint | orb stop 0 → brand hue at the orb's lightness | decorative |
| logo | `assets.logoMark`, `assets.logoFull` (existing vetting in `acceptImage`/`BrandLogo`) | |
| workspace name | `product.name` (≤ 80 chars, existing) | shown as the workspace name, never as the app name |
| display font | `typography.fontFaces` same-origin `.ttf/.otf` (existing `mapFonts`) | display/title/headline roles only |

### 3.2 What always stays Agent Zefir

These never come from the pack:

- every neutral: `background`, `surface*`, `elevated`, `text*`, `separator*`, `scrim`,
  `skeleton`, `bubbleAgent*`, `chip`, `chipText`
- `focus`: it stays coat blue, so a focus ring never disappears against an accent-filled
  control
- the status colours: `success`, `warning`, `danger`, `badge`, `secondary`
- spacing, radii, elevation, type scale, motion, haptics, icons, the mascot, the app
  icon set and the app name.

**Reason:** the pack's neutrals are tuned for the web app. For example, the default dark
background `#0E131D` is a blue-black and would fight the plum night palette. All AA proofs
in §2 depend on fixed neutrals. Fixed neutrals also keep security-relevant UI (the connect
confirmation, approvals, lock) recognisable on every server.

**Change from today** (`src/theme/brandPack.ts` `TOKEN_MAP`, `use-palette.ts`):

- Stop mapping `background.default`, `background.secondary`, `text.secondary`,
  `text.primary`, `border.lines` and `error.main` into the palette.
- Keep their parsing out of the persisted theme, so `restoreBrandTheme` re-validates
  less.
- Update `brandPack.test.ts` and `brandStore.test.ts` accordingly.

### 3.3 Automatic contrast correction (replaces "drop if weak")

Today a failing token is dropped and the app falls back. The new behaviour corrects the
brand colour instead, so a brand still shows:

```
correctAccent(brand, scheme):
  S = worst-case set for scheme = backgroundGradient stops (incl. tinted stop) ∪ background
      ∪ surface ∪ elevated ∪ (surfaceGlass composited over each of those)
  (L, C, H) = OKLCH(brand)
  step = scheme == light ? -0.004 : +0.004                // darken on light, lighten on dark
  while min(contrast(rgb(L,C,H), s) for s in S) < 4.5:  L += step   // gamut-map by reducing C
  on = text.button.primary if contrast(on, accent) ≥ 4.5
       else whichever of #FFFFFF / #000000 is higher
  while contrast(on, accent) < 4.5: push L further in the same direction  // mid-tone guard
  return accent, on
```

- The target is 4.5, not 3.0, because `accent` is also link text and the tinted label of
  plain buttons (iOS tint).
- Keep `parseColor`'s strictness: an `rgba()` with α < 1 is refused as an accent (no
  change from today).
- Implement OKLCH next to `src/theme/color.ts`. It is about 60 lines, and the proof script
  already contains a reference implementation.
- Correction runs at map time. The persisted theme stores the **corrected** values, and
  restore re-runs the correction (it is idempotent).

The correction was demonstrated on real inputs (from the script):

| Brand input | Light result (min over S) / on | Dark result / on |
|---|---|---|
| Elitea `primary.main` light `#C428DD` (3.90) | `#B810D1` 4.54 / white 5.21 | `#DA44F3` 4.53 / black 6.10 |
| Elitea `primary.main` dark `#6AE8FA` (1.26 light) | `#087784` 4.60 / white 5.27 | unchanged 10.79 / black 14.54 |
| lemon `#FFD400` | `#816B04` 4.53 / white 5.20 | unchanged 10.89 / black |
| navy `#0B1F44` | unchanged 14.15 / white | `#718BB9` 4.52 / black 6.10 |
| teal `#14B8A6` | `#0B796D` 4.61 / white | unchanged 6.26 / black |
| orange `#F97316` | `#B14F0C` 4.58 / white | unchanged 5.56 / black |
| Zefir rose `#B4306A` | unchanged 5.13 / white 5.88 | `#E15A90` 4.50 / black 6.07 |

The table is the correction against the untinted grounds (`correctAccent` alone, and what
`src/design/brand.test.ts` pins). When the brand also tints gradient stop 0, the app corrects
against that tinted stop as well, so a few results come out one step darker (Elitea light
`#B402CD`, teal light `#08786C`, orange light `#AE4D06`); every bar still holds.

The derived soft, bubble and tint values for each also pass. Each result had
`accentSoftOn/accentSoft` ≥ 6.0, `bubbleUserText` ≥ 5.2, and `textTertiary` on the
tinted gradient stop ≥ 4.54.

### 3.4 Scheme mapping

- **The OS setting rules.** Settings → Appearance offers "System / Light / Dark", stored
  per device (a convenience, so `localStorage`-equivalent secure prefs are fine). The pack
  never forces a scheme.
- Each scheme reads its own `schemes.light` or `schemes.dark` record.
  - If only one scheme yields an accent, derive the other from its hue with the
    correction above, starting from the same colour.
  - If neither does, use `brand.hue`.
  - If there is still nothing, keep Zefir rose.
- **`productDefault: true`** (the `X-Elitea-Brand-Layers: default` header, i.e. the server
  has no brand of its own) means **no colour override at all**.
  - The app keeps Zefir rose.
  - The workspace name and logo still show, because they identify the server.
  - **Change from today:** `usePalette` currently applies Elitea's default web colours.
- High contrast: the pack's optional `schemes.hc` is ignored. On iOS "Increase Contrast"
  (`AccessibilityInfo.isHighTextContrastEnabled` on Android), use the Zefir tokens with
  `textSecondary` → `textPrimary`, `separator` → `separatorStrong`, and glass → opaque.

### 3.5 Workspace identity is always visible

- The **workspace host** (e.g. `acme.elitea.ai`) and the logo mark (or a monogram tile
  when there is no logo) are visible on:
  - the connect confirmation (existing rule, system font)
  - sign-in
  - the Home header's workspace pill
  - the Settings → Workspace row
  - the lock screen
  - every approval card ("Running on acme.elitea.ai").
- The logo is never placed over or behind the mascot, and the mascot is never placed over
  the logo (§5).
- The workspace name is never shown as the app name. The app name stays "Agent Zefir" on
  the Settings → About screen, in system prompts, and in the iOS sign-in sheet
  (`CFBundleName`).

### 3.6 App icon variants (bundled, user-confirmed)

The rule is unchanged from `docs/design.md`: there are exactly two bundled icons, the
switch is user-confirmed, and icons are never downloaded.

| Variant | Name | Artwork | When offered |
|---|---|---|---|
| A (default) | "Zefir" | Zefirca head mark on the pastel pink→lavender→mint gradient | SaaS and default |
| B | "Zefir Night" (workspace / self-hosted) | the same framing on a neutral slate gradient (light `#F4F6F9`→`#D3DAE3`, dark `#3A4049`→`#16191E`); as built, see `docs/assets.md` | offered once when the connected workspace is not the SaaS host and `productDefault` is false |

- **No customer logo can be in an icon** (one store build, no per-customer builds).
- The brand pack schema has **no "suggest icon variant" field today**.
  - v1 rule: suggest variant B when `productDefault === false`.
  - An additive optional pack field, e.g. `product.appIconVariant: 'default' | 'workspace'`,
    is a server change to raise separately. It is not in scope here.
- **Formats, iOS:**
  - an Icon Composer `.icon` (wired as `ios.icon: './assets/zefirca.icon'`)
  - layers: background gradient (the document fill), the head mark foreground, centred
    within 0.40 of the canvas so the squircle corners never clip it
  - Default, Dark and Tinted (mono) appearances are produced by the system from the
    layers
  - variant B ships as an alternate icon (it needs a config plugin, such as
    `expo-alternate-app-icons` or our own plugin under `plugins/`, that adds
    `CFBundleAlternateIcons`)
  - iOS shows its own "You have changed the icon" alert.
- **Formats, Android:**
  - an adaptive icon: 108 dp canvas, the head mark inside the 66 dp safe circle (reach
    0.29 of the canvas; the safe circle is 0.306), gradient background layer
  - a **monochrome** layer, a flat single-colour glyph of the swirl head with the eyes,
    brows, smile and two swirl grooves cut out (reach 0.25), for themed icons on Android 13+
  - variant B is an `activity-alias` toggled via
    `PackageManager.setComponentEnabledSetting`
  - switching can close the app on some launchers, so the confirm copy says so.
- **The full-body mascot and the bust are too detailed below 60 pt.** Every icon uses the
  **head mark**: the head swirl, eyes, smile and headset with its orange antenna light,
  with the coat collar fading out below the chin. No hologram, no hands, no body.
- **Asset regeneration is required.** The source is a JPEG on a painted background with
  no alpha. Produce:
  - a 2048 px transparent master of the full figure, and a separate bust render (not a
    crop: the head needs to be re-centred and slightly larger)
  - a single-colour silhouette vector.
  - The pedestal stays only in the full-figure "hero" asset.
- **Splash:** replace `#208AEF`. The light splash uses `#FBF7F9` and the dark splash
  `#141118` (`expo-splash-screen` supports `dark`). The image is the bust with the
  hologram, fitted so every pixel is within 0.30 of the canvas: Android 12+ masks the
  splash icon to a circle of radius 1/3.

---

## 4. Components

Every component reads semantic tokens only, through a single `useTheme()` that replaces
`usePalette()`. `useTheme()` returns `{ color, type, space, radius, elevation, motion,
scheme, brand }`. Touch targets are at least 44 pt on iOS and 48 dp on Android, using
`hitSlop` where the visual is smaller.

### 4.1 Greeting header (Home)

- **Top row:** a user avatar (36) on the left. It is the user's photo if the server has
  one, else initials on `accentSoft`. On the right is a glass circular bell button (40)
  with a badge.
- **Workspace pill:** under the top row, or centred on wide layouts. It shows the logo
  mark (20) + workspace name, plus a chevron that opens Workspaces & projects.
- **Greeting:** display role, two lines. The first line is "Hello, {firstName}", where
  the name is set in `accent`. This matches the reference's coloured "Ask Me". The second
  line is "What can I help with?" in `textPrimary`.
  - Only the coloured span uses the accent, and the colour is not the only signal (the
    text is identical).
  - Both lines go in one accessible text node.
- **Badge:** the `badge` colour with a `badgeText` numeral. It shows a count up to 99 and
  "99+" above that. The label is "Notifications, 3 unread".
  - `badgeText` is white in both schemes. The iOS 26 native tab bar and bar-button badges
    draw a white numeral whatever text colour is requested, so `badge` must pass 4.5:1 with
    white (dark `#D2333A`, 4.91:1), not only with the requested `badgeText`.
- **iOS:** use a large-title-style custom header that collapses into an inline nav title
  ("Home") on scroll, with the same metrics as a UINavigationBar large title.
- **Android:** use a M3 top app bar (medium) with the greeting in the collapsing area,
  and the bell as an action icon button (48 dp).

### 4.2 Action card grid

- Use a 2-column grid of glass cards (radius 28, padding 16, min height 132, gap 12).
- **Card layout:**
  - top-left: a 40 pt icon tile (`accentSoft` circle, icon in `accentSoftOn`)
  - top-right: a 40 pt **circular arrow button** (glass, `arrowGo` icon, 1 px `separator`
    border)
  - bottom: the title (headline, 2 lines max).
- The **whole card is one button**. The arrow is visual only (`accessible={false}` on the
  arrow). The label is the card title plus a hint ("Starts a new chat").
- **v1 cards:**
  1. "New chat" (agent picker sheet)
  2. "Talk" (voice dictation)
  3. "Ask about a photo" (camera or library attach, then new chat)
  4. "Continue: {last conversation title}" (shown only when one exists).
- The reference's horizontal overflow scroll is **not** used, because it hides cards. At
  accessibility type sizes the grid becomes a 1-column list.
- **Pressed state:** scale .98 (spring) plus the pressed overlay. With reduced motion,
  only the overlay.
- **Android:** the cards are M3 "filled tonal" cards with a ripple. The arrow becomes a
  tonal icon button visual.

### 4.3 Chip row

- Horizontally scrolling pills, height 40 (hit 44/48), radius pill.
- **Chips:**
  - an **agent target** chip showing the current agent name (replaces the reference's
    "Smart Chat" and opens the agent picker; it is the existing `TargetChip`)
  - icon-only chips for mic, image and attach.
  - Icon-only chips are 40×40 circles with labels.
- **Selected chip:** `chipSelected` / `chipSelectedText`. Otherwise `chip` / `chipText`
  on glass.
- **iOS:** these are plain buttons. **Android:** M3 assist/filter chips (a filter chip
  with a check when selected).
- At accessibility sizes the row wraps.

### 4.4 History rows ("Recent chats")

- **Section header:** "Recent chats" (title3) with a trailing "See all" text button
  (accent, subheadline), 44 pt tall.
- **Row:** glass or surface container, radius 16, padding 12/16, min height 64.
  - leading: a 36 pt icon tile — the agent avatar or mascot bust (§5)
  - middle: the title (headline, 1 line) and below it the preview or agent name plus the
    relative time (footnote, `textSecondary`)
  - trailing: a 36 pt circular arrow button (visual only).
- **States:**
  - unread: a dot (8 pt, accent) plus a bold title, and "unread" in the label
  - pinned: a pin glyph
  - offline-cached: no extra glyph (the screen-level offline banner covers it).
- In the full Chats list, use plain grouped rows (iOS inset-grouped list style; Android
  M3 list items with 1 px `separator` dividers) rather than separate floating cards, for
  density.

### 4.5 Floating glass composer

- Sits above the keyboard (`KeyboardAvoidingView` / `react-native-keyboard-controller`),
  inset 12 from the screen edges and the safe-area bottom.
- Glass container, radius 28, min height 52, elevation e2.
- **Layout:** `[attach (+)] [multiline TextInput, body role, grows to 6 lines then
  scrolls] [mic | send | stop]`.
- **Trailing button morphs:**
  - empty input: **mic** (opens Voice)
  - with text or an attachment: **send** (a 36 pt `accent` circle with `accentOn` arrow)
  - while streaming: **stop** (a `textPrimary` circle with a square).
  - The morph is a cross-fade plus scale, with haptic light on send.
- **Attach** opens a menu:
  - iOS: a `UIMenu`-style context menu with Photo Library, Camera and File
  - Android: an M3 bottom sheet with the same items.
- The attachment strip shows above the input inside the composer (existing
  `AttachmentStrip`), as 56 pt thumbnails, each with a remove button (labelled).
- The mention and slash picker pops up above the composer as an `elevated` sheet card
  (existing `SuggestionStrip`).
- **Offline:** the placeholder becomes "Saved to send when online". The send button stays
  enabled (outbox), with a clock glyph.
- **Placeholder:** "Message {agent}…" in `textTertiary` (it passes 4.5 on glass).
- **iOS 26:** the composer is a Liquid Glass capsule. **Android:** a tonal surface. The
  keyboard's "send" return key is not used, so return inserts a newline. On Android with
  a hardware keyboard, Ctrl+Enter sends.

### 4.6 Message bubbles

- **User:**
  - right-aligned, max width 80%
  - `bubbleUser` / `bubbleUserText`, radius 22 with a 6 bottom-right corner
  - no avatar for me in 1:1. In a multi-person conversation, show other people's initials
    avatar (28) on the left of their bubbles, and use the existing self-identity to tell
    me apart.
- **Agent:**
  - left-aligned, full width minus the 28 avatar column
  - `bubbleAgent` with a 1 px `bubbleAgentBorder` (light). In dark mode it is a surface
    step with no border.
  - Markdown via the existing `SafeRenderer`. Code blocks use `surfaceSunken` with the
    code role, scroll horizontally, and have a copy button.
- **Agent avatar (28):**
  - the agent's own icon (from `icon_meta` when the client maps it; today `fetchAgents`
    does not read it)
  - **else the mascot bust**.
  - When several icon-less agents share a conversation, only the conversation's primary
    agent gets the mascot. The others get a monogram on `secondarySoft`, so speakers stay
    distinguishable.
  - The accessibility label is always the agent's name, never "Zefirca".
- The name line above the first bubble of a run shows the agent name (footnote,
  `textSecondary`) and the time.
- **Streaming:**
  - a pulsing caret at the end of the text
  - the "Thinking…" state shows the mascot bust with a 3-dot typing indicator (bouncy
    spring; static dots under reduced motion)
  - the screen-reader announcement is "{Agent} is responding". The text is announced on
    completion, not token by token.
- **Action row** (under a completed agent message, 36 pt icon buttons with a 44/48 hit
  area, `textSecondary`):
  - left group: copy, like, dislike, read aloud
  - right: regenerate.
  - **Like and dislike** are toggles (`accessibilityState.selected`). The selected one is
    filled in `accent`. Dislike opens an optional reason sheet.
  - **Read aloud** uses `expo-speech` (on-device TTS, a new dependency). While speaking,
    the button becomes stop.
  - **Regenerate** only shows on the last agent message, and is hidden offline.
  - **Copy** gives a "Copied" toast plus a haptic.
  - **Long-press** on any bubble opens the same actions as a context menu (iOS
    `ContextMenu`, Android a bottom sheet), plus Select text and Share. This is the
    accessible path, and the action row is the visible shortcut.
  - At accessibility sizes the row collapses to "…" (more).
- **Contract dependency:** like, dislike and regenerate need server endpoints in the
  client contract. Until those are present, hide the controls. Do not show dead buttons.

### 4.7 Tool-step and approval cards

- **Tool steps** (existing `ToolCalls` / `toolActivity`):
  - a compact row inside the agent message: a 20 pt `tool` icon, then
    "{tool name}" (callout), then a status (spinner, ✓ in `success`, ! in `danger`).
  - Several steps group into a disclosure "Used 3 tools" that is collapsed by default.
  - Tapping opens `ToolDetailSheet` (args and result in code style).
  - Status is never colour-only: an icon and text accompany it.
- **Approval card** (`PauseCard`, HITL):
  - a `surface` card with a 2 pt `warning` leading edge and the title "Approval needed"
    (headline)
  - "{Agent} wants to run {tool}" and "on {workspace host}" (always shown, §3.5)
  - an argument summary
  - buttons: **Approve** (filled `accent`), **Reject** (tonal `dangerSoft`/`danger`),
    **Details**.
  - On iOS, destructive or sensitive tools use a confirmation alert. On Android, a M3
    dialog.
  - **Never uses the mascot** (it would trivialise a security decision).
  - The approval card stays fixed-colour even when branded: accent buttons use the
    corrected brand accent, while the card chrome stays Zefir.
- **Ask-user / authorization / output-limit pauses** use the same card frame with their
  own forms.

### 4.8 Voice orb (dictation)

- **Presentation:** full-screen (iOS `fullScreenModal`; Android a full-screen dialog
  route).
- **Top bar:**
  - close (glass circle, 44)
  - a centre pill: "Listening" or "Paused", with a red dot when recording (both colour
    and text)
  - a trailing "…" menu: language, and "send automatically when I stop".
- **Title:** "Listening…" (title1) and below it the live transcript (body,
  `textSecondary`, up to 4 lines, scrolling).
- **Orb:** a 220 pt circle (160 on compact heights).
  - fill: a radial and linear blend of the `orb.stops` (pink → lavender → mint, from the
    reference), plus a specular `highlight`.
  - It scales between 1.0 and 1.12 with mic level, and breathes at idle.
  - It is decorative (`accessible={false}`). Status is conveyed by text.
  - **Reduced motion:** a static orb plus a ring whose width steps with level.
- **Bottom controls** (the reference's three): **keyboard** (back to typing), a large
  central **mic** (72 pt, filled `accent` while recording, tap to pause or resume), and
  **close**. The labels are "Switch to keyboard", "Pause listening" and "Cancel".
- **Cards** (from the reference's two glass cards): optional suggestions such as "Read
  last answer aloud". These are only shown when there is no transcript yet.
- **Haptics:** medium on start, light on stop.
- On stop, the transcript goes into the composer (or is sent automatically if that is
  enabled).
- **Dependencies:**
  - microphone permission (`app.config.ts` sets `microphonePermission: false` today and
    blocks `RECORD_AUDIO`)
  - speech recognition, either on-device (iOS `SFSpeechRecognizer`, Android
    `SpeechRecognizer`) or the server voice route.
  - This needs a privacy decision and a permission-copy update before the screen ships.

### 4.9 Empty states

- **Layout:** centred column on the gradient: the mascot (full figure 160 pt, or the bust
  96 pt in short containers), then the title (title2), a body line (callout,
  `textSecondary`), and one primary action (a filled pill button).
- **Copy, with the mascot's pose and prop:**
  - **no chats:** "Ask me anything" / "Start a chat or try your voice." / [New chat]
    (the mascot holds the gear hologram). On Home the action is left out: the New chat
    card is already on screen (screens.md §2).
  - **no results:** "Nothing found for '{q}'"
  - **no notifications:** "You're all caught up"
  - **offline with no cache:** "You're offline" (no mascot glow)
  - **error:** "Something went wrong" + [Try again] (the mascot without the hologram; never
    cartoonish copy for errors with data loss).
- The mascot is decorative here (`accessible={false}`), because the title carries the
  meaning.

### 4.10 Sheets

- **iOS:** native sheets (`presentation: 'formSheet'`, with detents `[0.5, 1]` and a
  grabber). Radius 32 is the system's. Use a nav bar with Cancel and Done text buttons
  when editing.
- **Android:** an M3 modal bottom sheet with a drag handle (32×4), a `scrim`, and a
  surface `elevated`. Full-screen dialogs are for longer forms.
- Used for the agent picker, participants, tool details, dislike reason, the attach menu
  (Android), and workspace & project switching.

### 4.11 Swipe actions (chat list)

- **iOS:** the leading swipe toggles Pin (`secondary`). The trailing swipe offers Archive
  (`textSecondary` fill) and Delete (`danger`). A full swipe performs Archive, not Delete.
- **Android:** M3 has no standard swipe actions. Offer a single trailing swipe to Archive
  with an undo snackbar, and long-press for the full menu.
- On both platforms, every swipe action is also in the long-press menu and in the
  accessibility actions (`accessibilityActions` with `onAccessibilityAction`), because
  swipes are invisible to VoiceOver/TalkBack.
- Delete always asks for confirmation (iOS action sheet, Android dialog). The threshold
  haptic is in §1.10.

### 4.12 Tab bar

- **Four destinations:** **Home**, **Chats**, **Discover**, **Settings**. Notifications
  are reached from the bell. New chat is reached from Home and from the Chats nav button.
- **iOS:** use native tabs (expo-router native tabs). On iOS 26 these give the Liquid
  Glass tab bar and its minimise-on-scroll behaviour. Use SF Symbols with filled variants
  when selected, and keep the tint as `accent`.
- **Android:** an M3 navigation bar, 80 dp, with an active indicator pill in `accentSoft`
  and the icon in `accentSoftOn`. Labels are always shown. Edge-to-edge (system nav bar
  transparent).
- **Badge:** the Chats tab shows the count of unread conversations.
- The tab bar is hidden on the chat thread, the voice screen and the connect/sign-in flow.
- **iPad and tablets:** a sidebar-adaptable tab view on iOS, and a navigation rail on
  Android at ≥ 600 dp.

### 4.13 Buttons, fields, banners (the base set)

- **Buttons:**
  - filled pill (`accent`/`accentOn`, height 50 iOS / 48 Android)
  - tonal (`accentSoft`/`accentSoftOn`)
  - plain text (`accent`)
  - destructive (`danger`)
  - icon circle (glass, 40).
  - The label is subheadline semibold, and is never all-caps on iOS. Android M3 uses
    sentence case too.
- **Text field:** `surfaceSunken` fill, a 1 px `separatorStrong` border (3:1), and a 2 pt
  `focus` border when focused. Errors use `danger` text plus an icon.
- **Banners:**
  - offline (existing `OfflineNotice`): a `warningSoft` strip with the `offline` icon and
    text
  - upgrade required (existing `UpgradeRequired`): a full screen.
- **Toasts:** iOS shows a small glass capsule above the composer. Android shows an M3
  snackbar.

---

## 5. Mascot usage rules ("Agent Zefirca")

| Where | Asset | Size (pt) | Accessibility |
|---|---|---|---|
| App icon | head mark (`design/tools/build.py`) | system | n/a |
| Splash | bust, fitted in the 1/3 circle | 200 dp image | n/a |
| Connect / welcome hero | full | 240 (180 compact) | decorative; the heading carries the meaning |
| Sign-in | bust | 64, next to the workspace logo **in a separate tile** | decorative |
| Home greeting avatar | **not used.** That slot is the user's avatar | n/a | n/a |
| Agent avatar fallback | bust | 28 (bubbles), 36 (rows), 48 (agent detail) | the label is the **agent's name** |
| Thinking indicator | bust + dots | 28 | "{Agent} is thinking" |
| Empty states | full / bust | 160 / 96 | decorative |
| Settings → About | full | 160 | "Agent Zefirca, the Agent Zefir mascot" (the only place it is named) |
| Lock screen / privacy cover | bust | 96 | decorative. The privacy cover must not show any content, and the mascot is neutral |

**Never:**

- Overlap, frame or replace the **workspace logo**. They sit in separate containers with
  at least 16 pt between them, and the logo always wins prominence on workspace-identity
  surfaces (connect confirm, Settings → Workspace).
- Replace an agent's own icon. The mascot is a *fallback* only.
- Appear on approval cards, destructive confirmations, security or permission prompts,
  or error states that involve data loss.
- Be recoloured, tinted with the workspace brand, inverted for dark mode, cropped
  mid-face, stretched, given a different outfit, or animated beyond the tilt and
  "thinking" bob.
- Speak as the agent. Copy is never written "as Zefirca" ("I'm Zefirca!"). The agent
  speaks for itself.
- Appear in more than one place on a screen (for example, an empty state *and* an avatar
  at the same time). In that case, the avatar falls back to a monogram.

**Specifics:**

- **Minimum size** is 24 pt. **Clear space** is 0.15 × the rendered width.
- **Dark mode** uses the same artwork plus a soft rim halo `rgba(255,255,255,.10)` so the
  pastel figure separates from night backgrounds. The full figure keeps the mint pedestal
  in both schemes.
- **Format:** the assets need re-rendering with alpha (see §3.6). Ship WebP @2x/@3x via
  `expo-image`, and mark the hero assets `priority="high"` on connect.
- **Naming:** the app is "Agent Zefir" and the mascot is "Agent Zefirca". The mascot's
  name appears only in About and in store screenshots.

---

## 6. Implementation notes (for the build plan)

These describe what the redesign touches. No code is changed by this document.

1. **Tokens:**
   - `src/constants/theme.ts` becomes a typed import of `design-tokens.json` (or a generated TS
     file), with semantic keys.
   - `Palette` widens from 8 keys to the semantic set.
   - `usePalette` becomes `useTheme`, which composes:
     - the scheme (System/Light/Dark preference)
     - the Zefir tokens
     - the corrected brand overrides (§3.1)
     - accessibility modifiers (reduce transparency, increase contrast, font scale).
2. **Brand mapping:**
   - In `src/theme/brandPack.ts`: shrink `TOKEN_MAP` to the accent family, replace
     drop-on-fail with `correctAccent` (OKLCH in `color.ts`), and honour
     `productDefault`.
   - `brandStore.ts` persists the corrected accent set (smaller, still under the 2 KB
     SecureStore budget).
   - Tests: there are existing suites for both files. Add a proofs test over
     `design-tokens.json`.
3. **New dependencies** (all Expo SDK 58 modules):
   - `expo-linear-gradient`, `expo-blur`, `expo-glass-effect`, `expo-symbols`,
     `expo-haptics`, `expo-image`, `expo-speech`
   - an alternate-icon config plugin.
   - Optionally `react-native-keyboard-controller` for the composer.
   - Microphone permission and speech recognition need a separate privacy decision.
4. **Navigation:** move to `(tabs)` with native tabs. Chat and voice are stack screens
   above the tabs (§4.12, screens.md).
5. **Contract gaps** that block parts of the reference UI:
   - feedback (like/dislike) and regenerate endpoints
   - the agent `icon_meta` mapping in `fetchAgents`
   - a user avatar URL in who-am-I
   - an optional pack hint for the icon variant.
   - Hide each affected control until its contract exists.

---

## 7. Foundation status and follow-ups

The foundation landed on `feat/design-foundation` (additive only; no screen was restyled):

- `src/design/tokens.ts`: the typed copy of `design-tokens.json`. `tokens.test.ts` fails on
  drift and re-proves every §2.1 pair in both schemes.
- `src/design/brand.ts` + `oklch.ts` + `contrast.ts`: the §3.3 correction and §3.4 scheme
  mapping (`resolveBrand`), with the §3.3 table as tests.
- `src/design/typography.ts`: the §1.7 ramp per platform, brand font on display/title roles.
- `src/design/resolve.ts` + `DesignProvider.tsx`: `useDesign()` composes scheme, Zefir
  tokens, the corrected brand accent and accessibility modifiers (Reduce Transparency,
  Increase Contrast, Reduce Motion, font scale). `<DesignProvider scheme>` is optional and
  only needed to force a scheme.
- `src/components/Mascot.tsx`: `<Mascot variant="avatar|bust|full|hero" size label decorative />`.
- `app.config.ts`: Zefirca icon (`assets/zefirca.icon`), adaptive icon, splash light/dark.

Follow-ups (not in the foundation):

- **F1 Alternate app icon.** Done with the entry/home redesign (§9): "Zefir Night" ships through
  `expo-alternate-app-icons` (iOS asset-catalog alternate, Android `<activity-alias>` with
  `plugins/withAlternateIconLinks.js`), offered once on connecting a self-hosted workspace and
  switched in Settings (`docs/assets.md`).
- **F2 Brand mapping in `src/theme/brandPack.ts`.** The pack mapper still applies the web
  neutrals to the old `Palette` and drops weak accents for the legacy theme. `useDesign()`
  ignores the neutrals and reads `BrandTheme.accentSource` (the pack's unfiltered accents,
  label colours and `brand.hue`, persisted and re-validated with the theme), so a weak accent
  is corrected and a missing scheme is derived per §3.4. Shrinking `TOKEN_MAP` to the accent
  family (§3.1) remains, with the screen adoption (F3).
- **F3 Screen adoption.** Screens move from `usePalette()` to `useDesign()` and the kit one by
  one. Done in the app shell (§8): the root mounts `<DesignProvider>`, and `usePalette()` is now
  a view of the design tokens, so no screen renders the pack's web neutrals any more.
- **F4 New dependencies** (§6.3): `expo-linear-gradient`, `expo-glass-effect`, `expo-symbols`
  (+ `@expo-google-fonts/material-symbols-rounded` for Android), `expo-haptics` and
  `react-native-keyboard-controller` landed with the shell; `expo-blur` and `expo-image` come
  with the screens that need them.

## 8. App shell and component kit (as built)

Landed on `feat/redesign-shell`:

- **One design app-wide.** `src/app/_layout.tsx` mounts `<DesignProvider>` (scheme, Zefir
  tokens, the corrected workspace accent, accessibility modifiers) above the navigation theme,
  so headers, the tab bar tint and the status bar follow the same tokens as the screens.
  `usePalette()` (`src/constants/use-palette.ts`) maps the eight legacy names onto the
  semantic tokens (plus `inputBorder` = `separatorStrong` and `field` = `surfaceSunken`).
- **Settings → Appearance** (System / Light / Dark, `src/state/appearance.ts`): a device item
  (`az.appearance`) applied with `Appearance.setColorScheme`, so the native chrome follows it.
- **Navigation.** Native tabs (Liquid Glass on iOS 26, minimise on scroll, sidebar on iPad; M3
  navigation bar with the `accentSoft` indicator on Android). Tab roots use large titles
  (`largeTitleOptions`), the Chats list a native header search (`searchBarOptions`). Header
  actions are `headerActionOptions`: native SF Symbol bar buttons with system badges on iOS 26,
  48 dp icon buttons with in-frame badges on Android and older iOS. The project switcher
  (`projectSwitcherOptions`) is the same chip on every platform: the project name and a
  chevron, truncated by the system, in the leading slot (iOS 26 wraps it in the glass capsule;
  a native bar button cannot show a title and a chevron together). The workspace host stays
  visible: the inline title (`WorkspaceTitle`) and, under a large title, `WorkspacePill`.
- **Keyboard and insets.** The chat uses `react-native-keyboard-controller`'s
  `KeyboardAvoidingView` with `automaticOffset` (no hard-coded header offset); the composer
  pads by the bottom safe area when the keyboard is down. Android: `predictiveBackGestureEnabled`
  is on, `softwareKeyboardLayoutMode: 'resize'`, edge-to-edge.
- **Type ramp everywhere.** No `fontSize`/`lineHeight` outside `src/design` and `src/ui`
  (`src/ui/noHardcodedFontSize.test.ts`); styles spread `ramp.<role>` or use `<Text variant>`.
  A spread role carries size, line height and face but no weight (a style sets its own), so
  Android's medium label roles never turn paragraph copy medium; `<Text variant>` keeps the
  role's weight.
- **Kit (`src/ui/`).** `Text`, `Icon` (SF Symbols / Material Symbols Rounded, one semantic map
  in `src/ui/icons.ts`), `haptic()`, `Button`, `IconButton`, `Badge`, `Chip`/`ChipRow`,
  `SegmentedControl`, `ListSection`/`ListRow`, `Card`, `ActionCard`, `ArrowButton`, `IconTile`,
  `GlassSurface`, `GradientBackground`, `EmptyState` (mascot), `Avatar` (initials tones proven
  4.5:1, agent mascot fallback), `sheetOptions` (native form sheet / M3 bottom sheet). Touch
  targets are tested (`src/ui/kit.test.tsx`).
- **Design gallery** (`/design-gallery`, `__DEV__` only, Settings → About → Design gallery):
  every component, with a preview scheme and sample brands (magenta, green, lemon) run through
  the real correction. `agentzefir://design-gallery?scheme=dark&brand=green` opens it directly.
  Two more `__DEV__` fixture routes: `/design-tabs/chats` (the native tab shell with sample
  chats: large title, header search, bar buttons with a badge, project switcher, workspace
  pill; the rows are the real `ConversationRow`, and `/design-tabs/settings` is the real
  `SettingsList` with sample values, so its screenshots show the shipped screens) and
  `/design-composer` (the real `Composer` on the keyboard, same keyboard handling as
  `/c/[id]`); both take `?brand=zefir|magenta|green|lemon`. `/design-sheet` shows `sheetOptions`.

## 9. Entry and Home (as built)

Landed on `feat/redesign-entry-home`:

- **Signed-out screens** share `EntryScreen` (`src/features/entry/`): the gradient ground, every
  safe area, keyboard-controller insets, one centred column.
  - Connect (`ConnectWelcome`): the mascot hero (bust at accessibility sizes and while typing),
    "Meet Agent Zefir", ONE primary action. The address field opens in place, already open when
    the address is known. The QR button is gone until a scanner exists (App Review 2.1).
  - Confirm (`ConnectConfirm`): no mascot; the workspace logo (or a monogram tile), its name, the
    host as the largest text, an inset list (connection, workspace, operated as, version). The
    pack's accent is not applied yet.
  - Sign-in (`SignInView`): the bust and the workspace logo in SEPARATE tiles, "Sign in to
    {name}", the host in a chip, one filled button in the workspace accent. Developer sign-in sits
    behind "Other sign-in options".
  - Lock (`LockView`): the bust, "Agent Zefir is locked", the workspace host and mark, "Unlock with
    Face ID / Touch ID / fingerprint" (`deviceAuth.biometry`), the wipe as a small plain button
    that confirms first. Update (`UpgradeView`) and the privacy cover use the same ground.
- **Home** is the first tab (`(app)/(tabs)/(home)`, the `/` route; Chats moved to `/chats`):
  `HomeView` + `homeHeaderOptions`. A transparent bar with the user's initials avatar (opens
  Settings) and the bell with its badge; the inline "Home" appears once the greeting scrolls
  away. The workspace pill reads "Name · Project" over the host (unbranded: "host · Project" on one
  line, so the first segment is always the workspace) and opens the project sheet. The
  greeting names the user ("who am I" `name`, kept in memory only). The chip row and four cards
  (New chat, Discover agents, Dictate a message, Ask about a file) start a chat; Dictate and
  Attach hand the new conversation a one-time action in memory (`IntentHandoff.then`), so a URL
  can never start the microphone or a picker. Up to five recent chats; the mascot empty state
  when there are none.
- **Chats** (`ConversationList`): inset-grouped sections by local day (Today, Yesterday, Previous
  7 days, Earlier; the contract has no pinned chats), swipe left to Delete (`deleteConversation`,
  confirmed first; also an accessibility action), none on the offline copy.
- **Switchers** are native sheets with detents (`sheetOptions`, Done for VoiceOver): projects by
  NAME under the active workspace (never "Project #id"), workspaces each with its mark in its
  OWN corrected accent (`WorkspaceSummary.brand`, `markTint`).
- **Hue-only brands.** A pack whose `brand.hue` left the product default's while its scheme
  records still state the default's `primary.main` is read the way the web brand editor writes
  it: the leftover primary is dropped and the hue drives the accent (`packAccentSource`); a pack
  that says `default` but carries another hue is that workspace's brand (`isProductDefault`).
  This was the "second workspace hue not applied".
- **Previews** (`__DEV__`): `/design-tabs/home` (also `?empty=1`, `?offline=1`),
  `/design-entry?screen=connect|address|add|confirm|sign-in|lock|lock-failed|upgrade`,
  `/design-workspaces`, `/design-projects`; all take `&brand=magenta|green|lemon` and render
  the real views (D2).

