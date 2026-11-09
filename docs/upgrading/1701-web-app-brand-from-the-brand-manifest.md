---
title: "Web app brand from the brand manifest (#1701, ADR-0052 W3)"
issue: 1701
date: 2026-11-08
adr: ADR-0052
order: 108
---

The web client now reads the brand from `brand/<id>/` at build time.
ELITEA's default pack, logos, favicon and titles are unchanged. Copy that
wrote the name in capitals now uses the product's short name: the tours,
the catalogue's page title ("Elitea Catalog") and the name the app
registers with remote MCP OAuth servers ("Elitea MCP Client"; clients
already registered keep theirs).

- **Another brand:** set `ELITEA_BRAND` before `npm run build`, or pass
  `--build-arg ELITEA_BRAND=<id or path>` to the web and elitea-main image
  builds. The default pack, logos, favicon, titles, product name and push
  notice title follow it. A served pack still overrides the default pack.
- **Support address:** the web client no longer falls back to a built-in
  support mailbox. A deployment that wants a support link sets
  `product.supportEmail` or `product.supportUrl` in its brand pack (Admin ›
  Branding); without one, no support link shows.
- **Translators:** messages name the product as `{{product}}`. Keep the
  placeholder; the app fills in the brand's name.
- **Contributors:** `apps/elitea-web/.prettierrc.json` sets the app's code
  style (`npm run format -- <file>`).
