---
title: "Documentation site moved to Astro Starlight"
issue: 1510
date: 2026-10-28
order: 39
---

* **The docs at `/docs/` are a separate app now (`apps/elitea-docs`).** The
  web image still serves them at `/docs/`, from a docs build stage, with a
  docs-only Content-Security-Policy (`docs-security-headers.conf`) that
  allows `'wasm-unsafe-eval'` for the Pagefind search. Page URLs are
  unchanged, except `/docs/integrations/toolkits/testIO_toolkit`, which is
  now `/docs/integrations/toolkits/testio_toolkit`; the web image's nginx
  answers the old URL with a 301 to the new one (GitHub Pages cannot). A
  deployment that
  replaces the image's nginx config must route `/docs/` the way
  `apps/elitea-web/nginx/spa.conf` does: static files with
  `try_files $uri $uri/ =404`, not a fallback to one HTML shell. The
  standalone and E2E compose stacks now build the image's `spa` target,
  which serves only a placeholder at `/docs/`; read the docs locally with
  `npm run dev` in `apps/elitea-docs`.
