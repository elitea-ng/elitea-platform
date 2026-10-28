# AGENTS: brand/

The Elitea AI brand guide: a static site for people (`index.html`) and agents
(`BRAND.md`, `llms.txt`, `tokens.json`, `tokens.css`). Published to GitHub Pages
at https://elitea-ng.github.io/elitea-platform/brand/ by
`.github/workflows/docs-pages.yml`, next to the docs but built from its own
sources. The docs never import it, and it never imports them.

## Where things live

| What | Where |
| --- | --- |
| Every rule, as prose | `brand/BRAND.md` (sections are `## NN Title`; the page's nav and layout come from them) |
| Every value | `libs/ts/client-core/src/brand/tokens.json`, the same file the web default pack, the docs SPA and the mobile app follow |
| The mark and the lockup | `libs/ts/client-core/src/brand/logo-mark.svg`, `logo-full.svg` (lockup wordmark = Sora 700 outlines, `fill="currentColor"`) |
| Fonts | `apps/elitea-web/public/brand/fonts/*.woff2` (copied into the build, not duplicated here) |
| Icon pack masters (downloads) | `brand/assets/icons/` (from the approved LE-03 icon pack) |
| Page shell, styles, behaviour | `brand/template.html`, `brand/styles.css`, `brand/brand.js` |
| Build and tests | `brand/scripts/build.mjs`, `brand/test/build.test.mjs` |

## Commands

```
node brand/scripts/build.mjs                 # → brand/dist (gitignored)
node --test brand/test/build.test.mjs        # build into a temp dir and check it
node libs/ts/client-core/scripts/gen-brand.mjs   # after editing tokens.json or a logo SVG
```

Node's standard library only: nothing to install.

## Rules

- **Write a rule once.** Prose goes in `BRAND.md`; numbers come from `tokens.json` through
  `{{placeholders}}` (an unknown one fails the build). Never paste a hex value that a
  token already holds into prose; generated tables carry them.
- **Contrast is computed, not claimed.** Add every new text or control pair to
  `tokens.json` → `contrast`; the build and `tokens.test.ts` fail below the minimum.
- **Every URL is relative**, so the site works under `/elitea-platform/brand/`, under any
  other path and from disk. The tests reject root-relative and unexpected external URLs.
- **No external fonts, scripts or CDNs.**
- The page is set in its own tokens (`--elitea-*`). Keep its CSS on them; the only literal
  colours are the fixed Ink/Paper tiles that show the logo on a known background.
- Interactive targets ≥ 24 × 24 px; visible focus (`signal`, 2 px, 2 px offset); no
  horizontal scroll at 390 px; motion off under `prefers-reduced-motion`.
- Canadian or British spelling as the rest of the product (`colour`, `catalogue`).
