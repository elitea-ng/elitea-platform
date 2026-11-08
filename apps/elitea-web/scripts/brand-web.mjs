// The web app's build-time brand (ADR-0052 decision 5, W3). Pure functions over a
// brand loaded by scripts/brand/load.mjs (the only reader of brand.json), used by
// vite.config.ts's `elitea-brand` plugin and tested in brand-web.test.mjs.
//
// ELITEA (unset ELITEA_BRAND) builds exactly what the committed files hold: the
// default pack is src/shared/brand/tokens/default.pack.json (elitea-main pins it
// byte for byte), and the logos are public/brand/ and src/shared/ui/icons/svg/.
// Any other brand gets its own default pack and logo files at build time.

/** The committed default pack's product fields a brand manifest replaces. */
const PRODUCT_FIELDS = ['name', 'shortName', 'docsUrl', 'supportUrl', 'supportEmail'];

/** Whether this build is the committed ELITEA build (nothing to replace). */
export function isDefaultBrand(brand) {
  return brand.id === 'elitea';
}

/**
 * The brand's compiled default pack (channel A).
 *
 * `pack.json` in the brand folder is used as it is. Otherwise the pack is derived
 * from the manifest over `reference` (the committed ELITEA pack): the product
 * names and links, `brand.hue` and `brand.onBrand`, and the scheme records the
 * manifest states. The reference's own token values are dropped, so every token
 * the brand does not state derives from its hue (toMuiPalette.ts) instead of
 * keeping ELITEA's colours.
 */
export function brandDefaultPack(brand, reference) {
  if (brand.has('pack.json')) return JSON.parse(brand.readText('pack.json'));
  const m = brand.manifest;
  const product = { ...reference.product };
  const values = {
    name: m.productName,
    shortName: m.shortName,
    docsUrl: m.docsUrl,
    supportUrl: m.supportUrl,
    supportEmail: m.supportEmail ?? undefined,
  };
  for (const field of PRODUCT_FIELDS) {
    if (values[field] === undefined) delete product[field];
    else product[field] = values[field];
  }
  const brandInput = { hue: m.brand.hue, ...(m.brand.onBrand === undefined ? {} : { onBrand: m.brand.onBrand }) };
  return {
    ...reference,
    id: m.id,
    product,
    brand: brandInput,
    schemes: { light: { ...m.brand.schemes?.light }, dark: { ...m.brand.schemes?.dark } },
  };
}

/** The `<svg …>` root tag of `svg`. */
function rootTag(svg) {
  const open = /^<svg\b[^>]*>/.exec(svg.trim());
  if (!open) throw new Error('not an SVG document (no <svg> root)');
  return open[0];
}

/** `svg` with its root tag's width and height set to 100% (the web copies of the mark). */
export function fillSized(svg) {
  const text = svg.trim();
  const open = rootTag(text);
  // As libs/ts/client-core/scripts/gen-brand.mjs writes ELITEA's copies: after the viewBox.
  const bare = open.replace(/\s(width|height)="[^"]*"/g, '');
  const sized = /\sviewBox="[^"]*"/.test(bare)
    ? bare.replace(/(\sviewBox="[^"]*")/, '$1 width="100%" height="100%"')
    : bare.replace(/^<svg/, '<svg width="100%" height="100%"');
  return `${sized}${text.slice(open.length)}\n`;
}

/**
 * The logo files the build writes for a brand: name → content. `public` names are
 * under the app's `brand/` folder (the default pack's `assets.*` paths and the
 * favicon link); `icons` are the SVGs the compiled logo components are made from.
 */
export function brandLogoFiles(brand) {
  const mark = brand.readText('assets/logo/logo-mark.svg');
  const full = brand.readText('assets/logo/logo-full.svg');
  const favicon = brand.readText('assets/web/favicon.svg');
  const withNewline = (text) => (text.endsWith('\n') ? text : `${text}\n`);
  return {
    public: {
      'logo-mark.svg': fillSized(mark),
      'logo-full.svg': withNewline(full),
      'favicon.svg': withNewline(favicon),
    },
    icons: { 'logo-mark-icon.svg': fillSized(mark), 'logo-icon.svg': withNewline(full) },
  };
}

/** HTML-escapes `text` for an element body. */
function escapeHtml(text) {
  return text.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}

/**
 * `html` with the committed product name in its `<title>` replaced by the brand's
 * (`Elitea AI` → productName, a leading `Elitea` → shortName). Unchanged for ELITEA.
 */
export function brandTitle(html, brand, reference) {
  const m = brand.manifest;
  return html.replace(/<title>([^<]*)<\/title>/, (_whole, title) => {
    const named = title.startsWith(reference.product.name)
      ? m.productName + title.slice(reference.product.name.length)
      : title.startsWith(reference.product.shortName)
        ? m.shortName + title.slice(reference.product.shortName.length)
        : title;
    return `<title>${escapeHtml(named)}</title>`;
  });
}

/** Where the build keeps a brand's generated files (git-ignored). */
export const BRAND_CACHE_DIR = '.brand';
