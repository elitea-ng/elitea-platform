# Brand repository template

This is the private template repository `elitea-ng/elitea-brand-template`
(GitHub › Use this template; ask EPAM for access). Its source is
`docs/brand-template/` in `elitea-ng/elitea-platform`: changes land there
first and are copied here.

A brand repository builds **released ELITEA code with its own brand folder, and
no patches** (ADR-0052 decision 6). It holds four things:

| Path | What it is |
|---|---|
| `brand/acme/` | The brand: `brand.json` and `assets/`. Rename `acme` to your brand id. |
| `upstream.json` | The ELITEA release this brand builds. |
| `.github/workflows/build.yml` | Builds iOS, Android, macOS and Windows from that release. |
| `.github/workflows/release-watch.yml` | Opens a pull request when ELITEA releases. |

The full guide, with the permanent ids, the NOTICE obligations, the signing
secrets and how to ask for a new manifest field, is in the ELITEA docs:
**Development › Build your own brand**.

## First steps

1. Rename `brand/acme` and set `BRAND_DIR` in `build.yml`.
2. Fill in `brand.json`: the names, the ids, the URLs, the hue.
3. Add `assets/`. To start from placeholders, run
   `node elitea/scripts/brand/placeholder-assets.mjs brand/<id>` on a checkout of
   the pinned release.
4. Check it: `node elitea/scripts/brand-check.mjs brand/<id>`.
5. Open a pull request. `build.yml` builds every platform unsigned.
6. Add the signing secrets, then run **Build the brand** by hand.

Replace `NOTICE` with your own notice, followed by the ELITEA release's
`NOTICE`.

Check the scripts with `node --test scripts/*.test.mjs`.
