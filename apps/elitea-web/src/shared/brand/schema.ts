import { z } from 'zod';

/**
 * `product.docsUrl` (issue W1b): the embedded docs SPA is same-origin, so
 * the compiled default pack states it as a root-relative path
 * (`/docs/`) rather than an absolute URL — `z.url()` alone rejects that
 * (it requires a scheme; verified against zod@4.4.3). A served pack may
 * still override it with a full absolute URL (a tenant's externally
 * hosted docs), so this accepts either shape while still rejecting a
 * protocol-relative `//host/...` (which `new URL()` would resolve against
 * an attacker-chosen scheme) and any non-`/`-rooted relative path.
 */
const relativeOrAbsoluteUrl = z.union([
  z.url(),
  z.string().regex(/^\/(?!\/)/, 'must be an absolute URL or a root-relative path (starting with a single /)'),
]);

/**
 * Tier 0 — the brand pack (spec §4.2). Reproduced from the spec; the only
 * additions are `shape.radiusPill` (S1 Part B) and the two optional
 * `product.supportEmail` / `product.senderName` contact fields (ADR-0024
 * WP8). No field was removed, widened or made optional.
 *
 * Contract notes that other units depend on (all verified against
 * zod@4.4.3 and mirrored field-for-field by the Go implementation in
 * `services/elitea-main/internal/api/v2/branding/pack.go`, unit W3):
 *
 *  - `.strict()` applies to the TOP LEVEL only. Nested objects keep zod's
 *    default "strip" mode, so an unknown nested key is dropped, not rejected.
 *  - `.optional()` means ABSENT, never `null`; no field in the schema is
 *    nullable.
 *  - `.default()` values are materialised on parse, so a parsed pack always
 *    carries `typography.baseSize`, `typography.scale`, `locale.default` and
 *    `locale.dateLocale` explicitly.
 *  - `schemes.light` / `schemes.dark` are OPEN records: token id -> colour.
 *    The id vocabulary is not part of the schema — it is the default pack's
 *    key set (`tokens/default.pack.json`), which is also the reference
 *    geometry `toMuiPalette` derives from. A pack may therefore ship a
 *    subset (or `{}`) and let `brand.hue` drive the rest.
 *
 * The derivation added by this unit needs NO extra schema field: `brand.hue`
 * is the only input it takes beyond the records themselves.
 */
export const BrandPack = z
  .object({
    $schema: z.literal('https://elitea.ai/schemas/brand-pack/1.json'),
    id: z.string().min(1), // 'default' | tenant slug
    version: z.string(), // semver of the pack itself
    product: z.object({
      name: z.string(), // 'Elitea'
      shortName: z.string(),
      tagline: z.string().optional(),
      // §4.2 writes `z.string().url()`. Zod 4 moved string formats to
      // top-level functions and deprecated the method form; `z.url()` is the
      // same schema (output `string`, same URL check), and the deprecated
      // spelling fails the D2 lint gate. Semantics are unchanged, so unit W3's
      // Go mirror (`optionalURL`) stays valid. Recorded as a §4.2 erratum.
      // `docsUrl` widens this to `relativeOrAbsoluteUrl` (above) rather than
      // the bare `z.url()` supportUrl below keeps — see that schema's doc
      // comment.
      docsUrl: relativeOrAbsoluteUrl.optional(),
      supportUrl: z.url().optional(),
      // ADR-0024 WP8: the two tenant-facing contact fields. Both optional and
      // both ABSENT-not-null, like every other optional here. `z.email()` is
      // the Zod 4 spelling of `z.string().email()` (same erratum as `docsUrl`
      // above). The Go mirror (`pack.go`) adds the same two fields; nested
      // objects strip unknown keys, so a pack from either side parses on the
      // other while the two land independently.
      supportEmail: z.email().optional(),
      senderName: z.string().optional(),
    }),
    assets: z.object({
      logoFull: z.string(), // data: URI or same-origin path
      logoMark: z.string(),
      favicon: z.string(),
      loginArt: z.string().optional(),
    }),
    typography: z.object({
      fontFamily: z.string(), // must resolve to a self-hosted @font-face
      fontFamilyMono: z.string(),
      baseSize: z.number().min(12).max(18).default(14),
      scale: z.number().min(1.05).max(1.5).default(1.2),
      // [ADR-0024 WP3] Additive and OPTIONAL: the self-hosted faces that make
      // `fontFamily` resolve. Each entry becomes one `@font-face` rule
      // (`fontFaces.ts`). `url` is a same-origin path — the Go mirror serves
      // `/api/v2/branding/assets/font/<digest>.woff2` — and the generator
      // drops anything else, so the schema stays a plain string here to keep
      // the two mirrors field-for-field identical. The default pack declares
      // none: with no face the browser falls through `fontFamily`'s stack.
      fontFaces: z
        .array(
          z.object({
            family: z.string().min(1),
            url: z.string().min(1),
            weight: z.string().optional(),
            style: z.enum(['normal', 'italic']).optional(),
          }),
        )
        .optional(),
    }),
    shape: z.object({
      radiusSm: z.number(),
      radiusMd: z.number(),
      radiusLg: z.number(),
      // [S1 Part B] Additive: the pill/circle shape MuiButton's icon-only and
      // `maxi` variants need — see buildTheme.ts and gen-brand-tokens.mjs for
      // the full rationale. radiusSm/Md/Lg are untouched.
      radiusPill: z.number(),
      density: z.enum(['comfortable', 'compact']),
    }),
    locale: z.object({
      default: z.string().default('en-GB'),
      dateLocale: z.string().default('en-GB'),
    }),
    // brand hue is scheme-INDEPENDENT: one hue, two lightness ramps derived from it
    brand: z.object({ hue: z.string(), onBrand: z.string().optional() }),
    schemes: z.object({
      light: z.record(z.string(), z.string()), // token id -> colour
      dark: z.record(z.string(), z.string()),
      hc: z.record(z.string(), z.string()).optional(),
    }),
  })
  .strict();

export type BrandPack = z.infer<typeof BrandPack>;

/** One scheme's token record: token id -> colour (or any CSS colour-bearing value). */
export type SchemeRecord = BrandPack['schemes']['light'];

/** The scheme-independent brand input `toMuiPalette` derives ramps from. */
export type BrandInput = BrandPack['brand'];

/** One self-hosted face from `typography.fontFaces` (ADR-0024 WP3). */
export type BrandFontFace = NonNullable<BrandPack['typography']['fontFaces']>[number];

/** The four asset slots of `pack.assets`. */
export type BrandAssetKey = keyof BrandPack['assets'];
