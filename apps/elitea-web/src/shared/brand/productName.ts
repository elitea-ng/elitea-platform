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
 * Pure: takes the pack as an optional argument, like `brandLinks.ts`.
 */
import { resolveBrandPack } from './channelC';
import type { BrandPack } from './schema';

/** The short product name, for running text ("Back to Elitea"). */
export function productName(pack: BrandPack = resolveBrandPack()): string {
  return pack.product.shortName;
}

/** The full product name, for titles ("Elitea AI"). */
export function productFullName(pack: BrandPack = resolveBrandPack()): string {
  return pack.product.name;
}
