/**
 * The Elitea product identity: the hand-set inputs of the DEFAULT brand pack.
 *
 * The default pack (`apps/elitea-web/src/shared/brand/tokens/default.pack.json`,
 * which elitea-main serves as `product_default.pack.json`) restates them by
 * hand, held equal by apps/elitea-web/src/shared/brand/__tests__/
 * defaultPackSync.test.ts. The mobile app
 * reads them directly. A workspace brand pack (ADR-0024 runtime branding)
 * replaces them at run time: these are the defaults only.
 *
 * Keep this file free of imports: it is plain data, read from TypeScript
 * source by the web, desktop and mobile builds alike.
 */

/** The product name and short name of the default pack. */
export const DEFAULT_PRODUCT = { name: 'Elitea', shortName: 'Elitea' } as const;

/** The default pack's `brand.hue`: the one scheme-independent brand colour. */
export const DEFAULT_BRAND_HUE = '#6ae8fa';
