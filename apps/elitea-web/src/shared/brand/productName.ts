/**
 * The product's name as the user sees it (ADR-0052 W3.2): every message,
 * label and title that names the product reads it from the brand pack, never
 * a literal. `shortName` ("Elitea") is the name used in running text;
 * `name` ("Elitea AI") is the full product name for titles.
 *
 * Resolution follows the pack: the served pack (channel C) when it is valid,
 * else the compiled default pack (channel A), which the build writes from
 * the selected brand (`ELITEA_BRAND`, vite.config.ts). The catalogues write
 * the name as `{{product}}`, filled in by `interpolation.defaultVariables`
 * (`shared/i18n/i18n.ts`).
 *
 * Takes the pack as an optional argument, like `brandLinks.ts`. Without one
 * it resolves the served pack once per pack object: dozens of module-scope
 * constants (tours, participant defaults) read the name at load, and
 * `resolveBrandPack()` trial-builds a theme on every call. The desktop app
 * publishes a new global object for each deployment, which resolves afresh.
 */
import { BRAND_PACK_GLOBAL, resolveBrandPack } from './channelC';
import type { BrandPack } from './schema';
import { DEFAULT_BRAND_PACK } from './tokens';

const resolved = new WeakMap<object, BrandPack>();

/** The resolved pack (channel C, else A), parsed once per served pack object. */
function currentPack(): BrandPack {
  if (typeof window === 'undefined') return DEFAULT_BRAND_PACK;
  const candidate: unknown = (window as unknown as Record<string, unknown>)[BRAND_PACK_GLOBAL];
  if (typeof candidate !== 'object' || candidate === null) return resolveBrandPack();
  const cached = resolved.get(candidate);
  if (cached !== undefined) return cached;
  const pack = resolveBrandPack();
  resolved.set(candidate, pack);
  return pack;
}

/** The short product name, for running text ("Back to Elitea"). */
export function productName(pack: BrandPack = currentPack()): string {
  return pack.product.shortName;
}

/** The full product name, for titles ("Elitea AI"). */
export function productFullName(pack: BrandPack = currentPack()): string {
  return pack.product.name;
}
