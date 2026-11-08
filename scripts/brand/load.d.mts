// Types for load.mjs, for the TypeScript generators that call it (apps/elitea-mobile/app.config.ts).
// Keep in step with schema.mjs (validateManifest) and load.mjs.

export interface BrandManifest {
  readonly formatVersion: 1;
  readonly id: string;
  readonly productName: string;
  readonly shortName: string;
  readonly publisher: string;
  readonly mobile: {
    readonly bundleId: string;
    readonly connectSchemes: readonly string[];
    readonly oauthRedirectScheme: string;
    readonly appGroup: string;
    readonly widgetBundleId: string;
    /** The numeric Apple ID of the brand's App Store Connect app, when it exists. */
    readonly ascAppId?: string | null;
  };
  readonly desktop: { readonly identifier: string };
  readonly linkDomain: string | null;
  readonly webUrl: string;
  readonly privacyUrl: string;
  readonly supportUrl: string;
  readonly supportEmail: string | null;
  readonly docsUrl: string;
  readonly brand: {
    readonly hue: string;
    readonly onBrand?: string;
    readonly schemes?: { readonly light?: Readonly<Record<string, string>>; readonly dark?: Readonly<Record<string, string>> };
  };
  readonly eas: { readonly projectId: string | null; readonly owner: string | null };
  readonly updates: { readonly mode: 'on' | 'off'; readonly codeSigningCertificate?: string };
  readonly push: { readonly relay: string };
}

export interface Brand {
  readonly dir: string;
  readonly id: string;
  readonly manifest: BrandManifest;
  readonly assetsDir: string;
  /** The absolute path of a file in the brand folder. */
  path(rel: string): string;
  /** Whether a file in the brand folder exists. */
  has(rel: string): boolean;
  /** A file of the brand folder as UTF-8 text. */
  readText(rel: string): string;
  /** A brand folder file as a `/`-separated path relative to the directory `from`. */
  relativeTo(from: string, rel: string): string;
}

export declare const REPO_ROOT: string;
export declare const BRANDS_DIR: string;
export declare const DEFAULT_BRAND_ID: string;
export declare const BRAND_ENV: string;
export declare class BrandError extends Error {
  readonly problems: readonly string[];
}
export declare function resolveBrandDir(value: string | undefined, options?: { cwd?: string }): string;
export declare function loadBrandFrom(dir: string): Brand;
export declare function loadBrand(options?: { env?: Record<string, string | undefined>; cwd?: string }): Brand;
