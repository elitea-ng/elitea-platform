# CI: the sample-brand output scan (#1707, ADR-0052 W4)

`scripts/ci/brand-output-scan.mjs` scans a client built with
`ELITEA_BRAND=sample` and fails on a product word (`Elitea`, `ELITEA`, `EPAM`)
or an ELITEA identifier from `brand/elitea/brand.json`.

- **Who is affected:** contributors to the web, mobile and desktop clients.
  A new user-facing "Elitea" string now fails ci-web, ci-mobile or ci-desktop.
  Read the name from the brand instead (web: `productName()`; mobile: the
  brand constants; desktop: the brand overlay), or add a reviewed `ALLOWED`
  entry with its reason.
- **Operators:** nothing to do.
