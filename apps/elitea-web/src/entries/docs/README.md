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
generated `id`, i.e. the heading text, lowercased and hyphenated).

### Components available in every page

`Note`, `Tip`, `Info`, `Warning`, `Check` (callouts) · `Card`, `CardGroup
cols` · `Accordion title`, `AccordionGroup` · `Tabs`, `Tab title` · `Steps`,
`Step title` · `Badge` · `Frame caption` · `Screenshot id alt` · `Icon
name` · plus a fenced ` ```mermaid ` block for diagrams (not a component —
the build turns the fence into a `<Mermaid>` element for you). See
`components/index.ts` for the exact prop shapes.

## Adding a screenshot

1. Add an entry to `shots.manifest.ts`: `{ id, route, viewport, selector?,
   actions?, mask? }`, where `route` is a real path in the running app
   (e.g. `/app/agents-hub`).
2. Reference it in a page: `<Screenshot id="your-id" alt="..." />`. With no
   image committed yet, this renders a labelled placeholder instead of a
   broken `<img>` — nothing breaks while the capture is pending.
3. Capture it with `apps/elitea-web/scripts/docs-shots.ts` (a later unit;
   it drives Playwright against the e2e stack) — or, until that lands, save
   a WebP by hand as `content/img/<id>.webp`, ≤250 KB, 1440×900, light theme.
4. `docs-content.test.ts` checks every committed image is ≤250 KB and
   referenced by some page, and every `Screenshot id` used has a manifest
   entry. `scripts/binary-allowlist.txt` already allows
   `apps/elitea-web/src/entries/docs/content/img/*.webp`.

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
