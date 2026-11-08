# brand/sample: the neutral test brand

**Never published.** No store listing, signing identity, push credential or
release uses this folder. It exists so that CI and the docs can build every
client under a brand that is not ELITEA (ADR-0052 decisions 2 and 11): built
with `ELITEA_BRAND=sample`, no output may contain an ELITEA name, mark or
identifier.

- `brand.json`: neutral names and ids under `example.com` (RFC 2606).
- `assets/`: a plain teal disc on slate at every size `brand-check` requires.
  No ELITEA mark. Regenerate (deterministic) with

  ```
  node scripts/brand/placeholder-assets.mjs brand/sample
  ```

- Check: `node scripts/brand-check.mjs brand/sample`.

The format is described in the docs: Development → Brands
(`apps/elitea-docs/src/content/docs/development/brands.mdx`).
