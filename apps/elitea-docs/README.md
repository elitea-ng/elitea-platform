# elitea-docs

The Elitea AI documentation site, built with Astro Starlight. The web image
serves it at `/docs/`, and GitHub Pages publishes it too.

```bash
npm ci
npm run dev          # http://localhost:4321/docs/
npm run check        # nav, front matter, components, screenshots
npm run build && npm run check:dist   # links, anchors, CSP hashes
```

How to add a page, the components a page can use, screenshots and the CSP
rewrite are covered on the site itself:
[`src/content/docs/development/docs-site.mdx`](src/content/docs/development/docs-site.mdx).
