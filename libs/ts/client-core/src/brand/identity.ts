/**
 * The Elitea product identity: the hand-set inputs of the DEFAULT brand pack.
 *
 * `apps/elitea-web/scripts/gen-brand-tokens.mjs` reads these when it writes
 * the default pack (`apps/elitea-web/src/shared/brand/tokens/default.pack.json`,
 * which elitea-main serves as `product_default.pack.json`). The mobile app
 * reads them directly. A workspace brand pack (ADR-0024 runtime branding)
 * replaces them at run time: these are the defaults only.
 *
 * Keep this file free of imports: Node runs it as TypeScript (type stripping)
 * from the web generator.
 */

/** The product name and short name of the default pack. */
export const DEFAULT_PRODUCT = { name: 'Elitea', shortName: 'Elitea' } as const;

/** The default pack's `brand.hue`: the one scheme-independent brand colour. */
export const DEFAULT_BRAND_HUE = '#6ae8fa';
