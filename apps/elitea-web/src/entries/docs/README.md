# Elitea docs (embedded)

The product documentation SPA, built as a fifth Vite mode (`docs`) of this
app alongside the main SPA, admin module, maintenance splash, and brand
previewer. It replaces the previous externally-hosted Mintlify site: the
content lives here, in this repository, and describes the current platform.

## Layout

```
src/entries/docs/
  index.html, 404.html   entry HTML (404.html is a copy, for GitHub Pages)
  main.tsx, App.tsx      shell: top bar, left nav, content column, TOC, prev/next
  router.ts              base-aware History API router (no routing library)
  nav.ts                 the nav tree: tabs → groups → pages
  content/**/*.mdx        the pages
  content/img/*.webp      committed screenshots (see "Adding a screenshot")
  components/            the MDX component contract (Note, Card, Tabs, …)
  mdx/remark-mermaid.ts  turns ```mermaid fences into <Mermaid chart>
  search.ts, content-registry.ts   client-side search / page loading
  shots.manifest.ts      typed manifest the screenshot capture script reads
  docs-content.test.ts   structural checks (nav↔files, links, images, …)
```

## Adding a page

1. Create `content/<slug>.mdx` with frontmatter:
   ```mdx
   ---
   title: Page title
   description: One sentence, shown in search results and link previews.
   ---

   Page content in MDX — Markdown plus the components below.
   ```
2. Add it to `nav.ts`, under the right tab/group, as
   `{ kind: 'page', slug: '<slug>', title: '...' }`.
3. Run `npm run test:unit -- src/entries/docs` — `docs-content.test.ts`
   fails if the file and the nav entry do not both exist (the nav↔files
   bijection), if an internal link or heading anchor does not resolve, or
   if frontmatter is missing.

Internal links are root-relative slugs with no extension:
`[Quick start](/quick-start)`, or `[a heading](/quick-start#sign-in)` for a
specific section (the anchor must match a real heading — `rehype-slug`'s
generated `id`, i.e. the heading text, lowercased and hyphenated). Write them
exactly like that — no `/docs/` or `/elitea-platform/` prefix, and no
`toPath()` call — the `a` override in `components/index.ts`
(`DocsLink.tsx`) prefixes the docs base for you at render time, the same way
`Card`'s own `href` does (`Card.tsx`), so a page never has to know whether it
is served at `/docs/` (nginx) or `/elitea-platform/` (GitHub Pages, via
`DOCS_BASE`). A bare `#heading` anchor (same page) and an external
`https://…`/`mailto:…` link are both left untouched — the latter always
opens in a new tab (`target="_blank" rel="noopener"`, added automatically).
A raw `<img src="/…">` gets the same base treatment (`DocsImage.tsx`), but
the contract prefers `<Screenshot id alt>` (below) — its `src` comes from a
Vite build-time asset URL, which already carries the base on its own.

### Components available in every page

`Note`, `Tip`, `Info`, `Warning`, `Check` (callouts) · `Card`, `CardGroup
cols` · `Accordion title`, `AccordionGroup` · `Tabs`, `Tab title` · `Steps`,
`Step title` · `Badge` · `Frame caption` · `Screenshot id alt` · `Icon
name` · plus a fenced ` ```mermaid ` block for diagrams (not a component —
the build turns the fence into a `<Mermaid>` element for you). See
`components/index.ts` for the exact prop shapes.

## Adding a screenshot

1. Add an entry to `shots.manifest.ts`: `{ id, route, viewport, selector?,
   actions?, mask?, persona?, theme?, notes? }`, where `route` is a real path
   in the running app (e.g. `/app/agents-hub`). If you are staging content in
   a scratchpad rather than editing this file directly, drop a
   `shots.<batch>.json` there instead (same shape, `viewport` may also be
   spelled `"1440x900"`) and see "Refreshing screenshots" below for how it
   gets folded in.
2. Reference it in a page: `<Screenshot id="your-id" alt="..." />`. With no
   image committed yet, this renders a labelled placeholder instead of a
   broken `<img>` — nothing breaks while the capture is pending.
3. Capture it with `apps/elitea-web/scripts/docs-shots.ts` — see "Refreshing
   screenshots" below — or, in a pinch, save a WebP by hand as
   `content/img/<id>.webp`, ≤250 KB, 1440×900, light theme.
4. `docs-content.test.ts` checks every committed image is ≤250 KB and
   referenced by some page, and every `Screenshot id` used has a manifest
   entry. `scripts/binary-allowlist.txt` already allows
   `apps/elitea-web/src/entries/docs/content/img/*.webp`.

## Refreshing screenshots

`apps/elitea-web/scripts/docs-shots.ts` is the repeatable capture driver
(embedded-docs unit W3). It reads `shots.manifest.ts`, signs in to a
**running** e2e stack the same way the Playwright suite does (reusing
`playwright.config.ts`'s `STORAGE_STATE`/`BASE_URL`, not a second login
implementation), and for every shot: sets the viewport, switches the app to
light theme through its own toggle, navigates to `route`, waits for network
idle and a quiet DOM (no fixed sleeps), runs any `actions`, blanks any `mask`
selectors, captures a PNG, and re-encodes it to WebP at ≤250 KB under
`content/img/<id>.webp`.

### 1. Bring up the e2e stack

```bash
# from apps/elitea-web — never the elitea-standalone compose project
bash scripts/e2e-stack.sh up
bash scripts/e2e-stack.sh seed
```

This stack (compose project `elitea-e2e`) is separate from
`elitea-standalone` and safe to run alongside it, **except** that both
default to the same OIDC mock port (9400). If `elitea-standalone` (or
anything else) already holds 9400, pass a free one through and use it for
every command below:

```bash
E2E_OIDC_PORT=9401 bash scripts/e2e-stack.sh up
E2E_OIDC_PORT=9401 bash scripts/e2e-stack.sh seed
```

The stack serves the app at `http://localhost:8082` by default (`E2E_PORT`
to change it; CI uses 8080). Podman locally, `docker compose` in CI —
`scripts/e2e-stack.sh` auto-detects which.

### 2. Merge staged shots (if a writer dropped any)

```bash
node scripts/docs-shots-merge.mjs /path/to/staging/dir
```

Folds every `shots.<batch>.json` in that directory into `shots.manifest.ts`,
deduping by `id`; an `id` defined differently in two places is a hard error
and nothing is written. Safe to re-run once the conflict is fixed.

### 3. Capture

```bash
# everything in the manifest
npx tsx scripts/docs-shots.ts --base-url http://localhost:8082

# just the ids you touched
npx tsx scripts/docs-shots.ts --only chat-home,agents-hub --fail-on-missing

# via the npm script (same thing)
npm run docs:shots -- --only chat-home --fail-on-missing
```

Flags: `--only <id,id,…>`, `--base-url <url>` (defaults to
`playwright.config.ts`'s `BASE_URL` / `PLAYWRIGHT_BASE_URL`), `--persona
<member|admin|chat>` (default for shots that do not name one), and
`--fail-on-missing` (exit non-zero if any requested shot produced no file —
useful for a targeted `--only` run, not recommended for a full run while the
manifest still holds writer-staged entries whose route isn't capturable yet,
e.g. a template placeholder like `/chat/:conversationId`).

If no persona's `STORAGE_STATE` file exists yet (a fresh stack), the script
runs `npx playwright test --project=setup` once, first — the same sign-in
every journey and the `@visual` suite already trust, not a second
implementation of OIDC.

The script prints a final `id | bytes | route` table. Commit the resulting
`content/img/*.webp` files normally; they are pre-allowlisted in
`scripts/binary-allowlist.txt`.

### 4. Verify

```bash
npm run test:unit -- src/entries/docs   # size/reference checks in docs-content.test.ts
npm run build:docs                       # confirms Vite bundles the new images
bash scripts/no-binaries-check.sh        # confirms nothing landed outside the allowlist
```

### CI

`.github/workflows/ci-web-e2e.yml`'s `docs-shots` job runs the same steps
against a fresh stack on `workflow_dispatch` (input `only`, passed straight
to `--only`) and uploads `content/img/*.webp` as an artifact — it never
commits. Download the artifact, look at the images, commit the ones you mean
to change, the same discipline the `visual` job's baseline-regeneration mode
follows.

## Running the build

```bash
# from apps/elitea-web
npm run build:docs              # runs the search-index generator, then vite build --mode docs
DOCS_BASE=/docs/ npm run build:docs   # explicit base (this is also the default)
npm run typecheck
npm run lint
npm run test:unit -- src/entries/docs
```

`build:docs` first runs `node scripts/docs-build-search-index.mjs`, which
walks `content/**/*.mdx` and (re)writes `search-index.generated.json` —
committed like `src/routeTree.gen.ts`, so `tsc --noEmit` and a plain
`npm run dev` never trip over a missing file, but it goes stale the moment
you edit a page's text without re-running the script. Re-run it by hand
after editing content between builds; `npm run build:docs` always refreshes
it before building.

`DOCS_BASE` controls where the built assets expect to be served from:
nginx serves the build at `/docs/` (the default, if `DOCS_BASE` is unset),
and the GitHub Pages workflow builds with `DOCS_BASE=/elitea-platform/`
since Pages publishes this repository's site under that path.

To preview a production build locally without the full deploy stack:

```bash
npm run build:docs
npx vite preview --outDir dist/docs --base /docs/
# or: cd dist/docs && python3 -m http.server 4173
```
