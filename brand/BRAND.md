# Elitea AI — brand guide

Version {{version}}. The identity of Elitea AI, the unified AI platform: the name, the LE-03 mark, colour, type, voice and the design tokens products read. Written for people and for agents: every rule here is something you can check, and every value comes from one file, [`tokens.json`](tokens.json).

The guide is restraint, not more colour: one primary, signals that each have a single job, and neutrals doing the work.

## 01 Name

- Write **Elitea AI**: two words, capital E, "AI" in capitals. Never "EliteaAI", "elitea ai", "ELITEA" or "Elitea.ai".
- **Elitea** alone is fine in running text once the full name has appeared ("Open Elitea", "Elitea keeps…"), and as the short name where space is tight (app menus, the dock, the home screen).
- Domains are always lowercase: elitea.ai.
- The mark is **LE-03**: three stacked layers hinting at an E, the middle layer split into a bar and a dot. It replaced the gradient loop and the gradient orb; both are retired everywhere.

### The lockup

The mark plus the wordmark "Elitea AI", set in Sora 700 and supplied as outlines. Use the supplied lockup files and never re-typeset the wordmark. The wordmark takes the text colour of its surface (Ink on Paper, `#F2F4FA` on Ink); the mark keeps its colours.

- Minimum lockup width: 96 px. Below that, use the mark alone.
- Gap between mark and wordmark: 0.4 × the mark's height. It's built into the file.

## 02 Logo

The mark is four flat rounded rectangles on a 372-unit square: Signal Cyan on top, a Layer Violet bar and the Dot Orange square in the middle, Base Indigo at the bottom. The dot is part of the mark, and the only place orange gets to be big.

- **Clear space** on every side equals one layer's height (104 units, 28% of the mark). No type, edges or other logos inside it.
- **Minimum size**: 16 px for the mark on screen (favicon), 24 px in UI.
- **Variants**: coloured (on Ink, Paper or white), one-colour Ink, one-colour white. Nothing else.
- The coloured mark sits only on Ink, Paper or white. Never on cyan, violet, indigo or orange, where its own layers disappear.
- On photography or busy surfaces, put the mark on a solid Ink or white tile: the app-icon squircle (corner radius 114 on a 512 grid, mark inset 70).
- Never stretch, rotate, outline, shadow, gradient-fill or recolour the layers. Never redraw the geometry; the dot's right edge sits 8 units inside the bars by design.
- The retired gradient loop and gradient orb are not variants. Don't use them anywhere, including "just this once" social posts.

### App icons

| Surface | Variant | Notes |
| --- | --- | --- |
| macOS, Windows, desktop dock | coloured on Ink, squircle | `elitea-app-icon-1024.png` is the master |
| iOS | coloured on Ink, square, opaque | the system rounds the corners |
| Android adaptive | mark foreground on Ink background | monochrome layer = white mark |
| Web favicon | coloured on Ink tile (SVG) | reads on light and dark browser chrome |
| GitHub org / social | coloured on Ink | avatar 1024, social preview 1280×640 |
| macOS menu bar | black mark, template image | the system tints it |

## 03 Colour

Four colours, taken from the mark in layer order, and each one has exactly one job. These are rules, not suggestions: they stop the product drifting back into a different accent family per theme.

{{table:palette}}

### One colour, one job

- **Indigo is the single primary action colour in both themes**: buttons, primary actions, selected states, toggles on. White text on it is 5.97:1.
- **Cyan is the dark-theme signal**: links, active navigation, focus rings on Ink (8.92:1). On light, the signal role is Indigo and cyan appears only inside the mark.
- **Violet is the AI accent only**: model badges, AI feature markers, assistant presence. Never buttons, never large fills. Text in the AI role uses `aiText`.
- **Orange is the dot only**: a notification dot or a live/run marker. At most one per view. Counts and badges are Indigo or danger, never orange. If orange appears twice, it means nothing.
- **Neutrals carry the interface.** Sidebars, cards, inputs and text are Ink/Paper steps, never tinted brand colour.
- **At most two brand hues per screen** besides the mark, plus the one orange dot. A screen that needs a third hue has too much happening. Charts are the exception and follow the chart order below.
- **Flat fills only.** No gradients in UI chrome: no gradient buttons, text, borders, avatars, cards or orbs.
- **Semantic colours are not brand colours.** Success is green, warning is amber (never orange), danger is red, and each has a per-theme value that passes AA as text.

### Proportion

Neutrals about 78%, Indigo 12%, Cyan 5% (dark), Violet 3%, Orange 2%: the dot, almost never.

### Charts

Series take, in order: Indigo, Signal (cyan on dark, `#0E7490` on light), Violet, then `textMuted`. Semantic colours only for semantic series (errors, budget exceeded). Never orange.

### Banned pairs

{{table:banned}}

### Kill list: retired, do not reintroduce

{{table:kill}}

Also retired: the gradient orb, the rainbow loop logo, gradient text, and any per-theme accent swap. If one of these shows up in a build or a template, it's a bug against this guide.

## 04 Type

Three families with three jobs. All are SIL Open Font License and self-hosted (no font CDN: the desktop app works offline and the app's font policy is same-origin only).

| Family | Job | Weights |
| --- | --- | --- |
| **Sora** | Display and headings, the wordmark | 700, 800 |
| **Manrope** | Interface and body text | 400, 600 |
| **JetBrains Mono** | Anything a machine produced or will parse: IDs, URLs, hex, code, eyebrows | 400, 500 |

{{table:typescale}}

- **Sentence case everywhere**: "Choose the toolkit type", not "Choose The Toolkit Type". Headings, buttons, menu items, tabs.
- **Two weights per family, maximum.** Hierarchy comes from size and colour role, not five weights competing.
- Uppercase is reserved for mono eyebrows, tracked +0.16em.

## 05 Voice

Calm, precise and on the user's side. Elitea AI is infrastructure people trust with their data and credentials, and the words should sound like it.

- **Plain over playful.** Understatement beats exclamation marks.
- **Accurate first**: name the thing, name what happens next.
- **No hype words**: "supercharge", "revolutionary", "magic", "10x", "unleash". The pitch is control and competence.
- **Empty states name the state and the way out**: "No tags yet. Tags you add will appear here." Not "No tags to display."
- **Placeholders say what the box does**: "Search agents, chats, toolkits…", not "Let's find something amazing!"
- **Errors say what happened and what to do**, without blame and without exclamation marks: "Couldn't save the toolkit. Check the token and try again."
- No emoji in UI labels.

## 06 Tokens

Products read these tokens. One token set, same names in both themes: only the neutrals invert, and the brand roles keep their jobs. In the web app they are the default brand pack (`apps/elitea-web/src/shared/brand/tokens/default.pack.json`); a workspace brand pack (white-label, ADR-0024) replaces them at run time.

{{table:schemes}}

| Token | Value | Use |
| --- | --- | --- |
| `radiusSm` / `radiusMd` / `radiusLg` | {{shape}} | chips and small controls / buttons and inputs / cards, dialogs |
| space | {{space}} | 4 px grid |
| focus ring | {{focus}} | every interactive element, both themes |
| motion | {{motion}} | removed under `prefers-reduced-motion` |
| elevation (light) | {{elevationLight}} | cards, menus, dialogs |
| elevation (dark) | surface steps, no shadow | `bg` → `surface` → `elevated` |

```css
{{code:tokens.css}}
```

## 07 Accessibility

Every text pair below is measured (WCAG 2.2 relative luminance), not guessed; the build and the test suite recompute them from `tokens.json` and fail when one drops below its minimum.

{{table:contrast}}

- **Focus is always visible**: a 2 px ring in `signal` with a 2 px offset in the page colour. The offset matters on light, where `signal` and `primary` are the same Indigo: a ring flush on an Indigo button would vanish.
- **Control boundaries use `borderStrong`** (3:1 or better against every surface, WCAG 1.4.11). Hairline `border` is for dividers only, never the only edge of an input.
- **Indicators on dark surfaces**: Indigo on `surface` is 2.97:1, under the 3:1 non-text bar. A selected nav item therefore pairs the Indigo fill with white text, or uses a `signal` indicator bar; a toggle's "on" state relies on its white thumb, not the track alone.
- **Never colour alone**: status carries an icon or a word as well as a hue.
- Interactive targets are at least 24 × 24 px.
- Motion is removed under `prefers-reduced-motion: reduce`.

## 08 Guardrails

### Always

- One primary: Indigo in both themes, for every primary action.
- Let neutrals carry the interface; reach for a brand hue only when it has its one job to do.
- Before shipping a screen: at most two brand hues besides the mark, orange at most once.
- Use the supplied mark files and tokens. Never redraw, recolour or re-tokenise locally.
- Warnings in amber, errors in red: semantic colours, per-theme values.

### Never

- Reintroduce a kill-list colour.
- Use a banned pair.
- Put a gradient in UI chrome.
- Make orange a button, a warning, a badge or a large fill. It's the dot.
- Swap the accent family per theme. Dark and light are one system; only neutrals invert.

## 09 For agents

This guide is built to be read by machines as well as people. If you are an agent producing anything that carries the Elitea AI brand (UI, a slide, a page, an e-mail, a diagram):

1. Read this file, [`BRAND.md`](BRAND.md), first. [`llms.txt`](llms.txt) indexes every resource here.
2. Take colours, type and radii from [`tokens.json`](tokens.json) (W3C design-token format) or [`tokens.css`](tokens.css) (`--elitea-*` custom properties, light by default, dark under `[data-theme="dark"]` or the system preference). Never sample colours from screenshots.
3. Use the logo files under `assets/` as they are. The coloured mark needs Ink, Paper or white behind it.
4. Check your output against section 08: one primary, at most two brand hues, orange once, no gradients, no kill-list colours, sentence case, AA contrast.
5. In the Elitea platform repository the canonical sources are `libs/ts/client-core/src/brand/` (`tokens.json`, `logo-mark.svg`, `logo-full.svg`). Edit there and run `node libs/ts/client-core/scripts/gen-brand.mjs`. Never edit a generated copy.

## 10 Changes from v2

The v2 proposal was reviewed against WCAG 2.2, the running product and its own rules before it became this guide. The decisions it made stand; these are the gaps that were closed.

- **Semantic colours failed as text on light.** Success `#22C55E` (2.28:1), warning `#F59E0B` (2.15:1) and danger `#EF4444` (3.76:1) on white fail AA. Light now uses `#15803D`, `#B45309` and `#B91C1C`; dark keeps the bright values, with danger raised to `#F87171` (`#EF4444` is 4.28:1 on `elevated`).
- **White on danger fails** (3.76:1). Destructive buttons use `dangerFill` `#DC2626` (4.83:1) in both themes.
- **Input borders were invisible to the contrast rule.** `#E2E6F0` on white is 1.25:1 and `#232A45` on Ink 1.38:1, against the 3:1 that WCAG 1.4.11 asks of a control's edge. Added `borderStrong`.
- **The focus ring vanished on light primary buttons**, because `signal` = `primary` = Indigo there. The ring now has a 2 px offset.
- **Violet text was below AA in both themes** (4.37:1 on white, 4.47:1 on Ink). Added `aiText`: `#6447F0` on light, `#9A8CFF` on dark.
- **Indigo indicators on dark surfaces** (2.97:1 on `surface`) now have a written rule.
- **States were missing**: hover, pressed, disabled, selected, scrim and code tokens were added, all measured.
- **Internal contradictions were resolved**: "two weights per family" against a scale that used three Manrope weights; a 96 px lockup minimum against "below 24 px, drop the wordmark"; eyebrow tracking given as both 0.22em and 0.16em; 16.5 px body text against a 13–16 px scale.
- **Fonts came from a CDN.** They are now self-hosted, because the app's policy is same-origin only and the desktop app must work offline.
- **Missing foundations were added**: spacing, motion, elevation, chart order and icon-surface rules, so that agents don't have to invent them.
- **Personal and sample data was removed**: reviewer names, a real project id and a city.

## 11 Downloads

- Logo: [mark, coloured](assets/elitea-mark.svg) · [mark, one-colour Ink](assets/elitea-mark-ink.svg) · [mark, one-colour white](assets/elitea-mark-white.svg) · [app icon (SVG)](assets/elitea-app-icon.svg)
- Lockup: [on light](assets/elitea-lockup-light.svg) · [on dark](assets/elitea-lockup-dark.svg)
- App icon PNG: [coloured on Ink, 1024](assets/icons/elitea-app-icon-1024.png) · [coloured, transparent, 1024](assets/icons/elitea-mark-1024.png) · [one-colour, 1024](assets/icons/elitea-mark-bw-1024.png) · [white on Ink, 1024](assets/icons/elitea-app-icon-inverted-1024.png) · [Windows .ico](assets/icons/elitea.ico) · [GitHub social preview](assets/icons/elitea-social-1280x640.png)
- Tokens: [tokens.css](tokens.css) · [tokens.json](tokens.json)
- Fonts (OFL): [Sora](fonts/sora.woff2) · [Manrope](fonts/manrope.woff2) · [JetBrains Mono](fonts/jetbrains-mono.woff2)
- This guide as Markdown: [BRAND.md](BRAND.md)
