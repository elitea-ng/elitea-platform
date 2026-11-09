// Types for brand-web.mjs (vite.config.ts imports it). Keep in step with it.
import type { Brand } from '../../../scripts/brand/load.mjs';

export declare const BRAND_CACHE_DIR: string;
export declare function isDefaultBrand(brand: Brand): boolean;
export declare function brandDefaultPack(
  brand: Brand,
  reference: Record<string, unknown> & { product: Record<string, unknown> },
): Record<string, unknown> & { product: { name: string; shortName: string } };
export declare function fillSized(svg: string): string;
export declare function brandLogoFiles(brand: Brand): {
  public: Record<string, string>;
  icons: Record<string, string>;
};
export declare function brandTitle(
  html: string,
  brand: Brand,
  reference: { product: { name: string; shortName: string } },
): string;
