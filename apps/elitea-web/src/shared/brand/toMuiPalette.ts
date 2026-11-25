import type { PaletteOptions } from '@mui/material/styles';

import type { DeriveContext } from './color';
import { hueOf, rehue } from './color';
import type { BrandInput, SchemeRecord } from './schema';
import { BRAND_ANCHOR_TOKEN, DEFAULT_BRAND_PACK, LEGACY_ROLES, ROLE_GROUP } from './tokens';

/** The two schemes a pack always carries. `hc` is optional and not built yet. */
export type SchemeName = 'light' | 'dark';

/**
 * Token ids the default pack states for a scheme — the derivation catalogue.
 * A pack that omits a token gets the reference value re-hued; a pack that
 * adds an id outside the catalogue gets it verbatim.
 */
export function catalogueFor(scheme: SchemeName): readonly string[] {
  return Object.keys(DEFAULT_BRAND_PACK.schemes[scheme]);
}

/** Hue of `scheme`'s reference accent — what a pack's `brand.hue` replaces. */
function anchorHueFor(scheme: SchemeName): number {
  const anchor = DEFAULT_BRAND_PACK.schemes[scheme][BRAND_ANCHOR_TOKEN];
  if (anchor === undefined) {
    throw new Error(`default pack has no ${BRAND_ANCHOR_TOKEN} in the ${scheme} scheme`);
  }
  return hueOf(anchor);
}

/**
 * Hue rotation applied to `scheme`'s brand family for a given pack hue.
 *
 * The reference is the SCHEME'S OWN accent, not a single global one. That is
 * what makes one `brand.hue` land both schemes on the same hue even though the
 * baseline's two accents are 104° apart (§4.1 Blocker 1).
 */
export function hueDeltaFor(scheme: SchemeName, brand: BrandInput): number {
  return hueOf(brand.hue) - anchorHueFor(scheme);
}

/** Everything one scheme's derivation needs, computed once per build. */
function deriveContextFor(scheme: SchemeName, brand: BrandInput): DeriveContext {
  return {
    deltaDeg: hueDeltaFor(scheme, brand),
    brandHueDeg: hueOf(brand.hue),
    anchorHueDeg: anchorHueFor(scheme),
  };
}

/**
 * Resolve one scheme's full token record: the pack's own values, plus a
 * derived value for every catalogue id the pack does not state.
 *
 * This is where §4.1 Blocker 1 is resolved. The default pack states every id
 * per scheme, so derivation is fully shadowed and rendering is byte-equal
 * to the baseline (N4). A tenant pack that states only `brand.hue` states
 * zero tokens, so every id is derived and the whole surface repaints —
 * accents, neutrals and gradients alike — from one field.
 *
 * A role (`role.<name>`) takes the first value the pack states differently
 * from the default pack: the role id itself, then each legacy id of its list
 * in `tokens/legacy-roles.json`, in list order (ADR-0062 decision 1.4). So a
 * hand-tuned deployment keeps its colours in the roles. If none differs, the
 * role resolves as every other id above: its stated value, or its default
 * re-hued (decision 1.6). "Stated with the default value" covers both kinds
 * of export: a tuned deployment's states only the ids it set, the product
 * default's states every id with its default, so nothing in it overrides a
 * role.
 */
export function resolveScheme(
  record: SchemeRecord,
  brand: BrandInput,
  scheme: SchemeName,
): Record<string, string> {
  const reference = DEFAULT_BRAND_PACK.schemes[scheme];
  const context = deriveContextFor(scheme, brand);
  const resolved: Record<string, string> = {};
  for (const id of catalogueFor(scheme)) {
    const stated = record[id];
    resolved[id] = stated ?? rehue(reference[id] as string, context);
  }
  for (const [role, legacyIds] of Object.entries(LEGACY_ROLES)) {
    const roleId = `${ROLE_GROUP}.${role}`;
    for (const id of [roleId, ...legacyIds]) {
      const stated = record[id];
      if (stated !== undefined && !sameColour(stated, reference[id])) {
        resolved[roleId] = stated;
        break;
      }
    }
  }
  for (const [id, value] of Object.entries(record)) {
    resolved[id] ??= value;
  }
  return resolved;
}

/** Hex case differs between the admin editor and the pack; the colour does not. */
function sameColour(a: string, b: string | undefined): boolean {
  return b !== undefined && a.toLowerCase() === b.toLowerCase();
}

/** Expand `a.b.c` -> nested objects. Throws on an id that shadows a group. */
export function unflatten(record: Record<string, string>): Record<string, unknown> {
  const root: Record<string, unknown> = {};
  for (const [id, value] of Object.entries(record)) {
    const parts = id.split('.');
    let node = root;
    for (let i = 0; i < parts.length - 1; i++) {
      const key = parts[i] as string;
      const next = node[key];
      if (next === undefined) {
        node[key] = {};
      } else if (typeof next !== 'object' || next === null) {
        throw new Error(`token id ${id} nests under the leaf ${parts.slice(0, i + 1).join('.')}`);
      }
      node = node[key] as Record<string, unknown>;
    }
    const leaf = parts[parts.length - 1] as string;
    if (typeof node[leaf] === 'object' && node[leaf] !== null) {
      throw new Error(`token id ${id} collides with a group of the same name`);
    }
    node[leaf] = value;
  }
  return root;
}

/**
 * Pack scheme record -> MUI palette (spec §4.2 tier 2).
 *
 * Deviation from the §4.2 snippet, deliberate and total: the snippet calls
 * `toMuiPalette(pack.schemes.light, pack.brand)`. The scheme NAME is a third
 * argument because it is needed twice — to set `palette.mode` (MUI's own
 * light/dark component logic reads it, even with CSS variables on) and to
 * pick the scheme's reference ramp and brand anchor. Nothing else differs.
 *
 * `palette.error` and `palette.success` are not special-cased here: the
 * default pack carries them as ordinary token ids (`error.main`, …), so they
 * arrive through the same path as every other token and are white-labelable
 * like every other token. What §4.2 mandates is that they EXIST, and the
 * contract test asserts they resolve to pack values rather than MUI defaults.
 *
 * The single cast is the boundary between the pack's open token vocabulary
 * and MUI's closed `PaletteOptions` type; `tokens/palette.augment.d.ts`
 * types the READ side (`theme.vars.palette.*`) so component authors never
 * touch an `any`.
 */
export function toMuiPalette(
  record: SchemeRecord,
  brand: BrandInput,
  scheme: SchemeName,
): PaletteOptions {
  const palette = unflatten(resolveScheme(record, brand, scheme));
  palette['mode'] = scheme;
  if (brand.onBrand !== undefined) {
    // `onBrand` is the foreground the pack wants on brand-coloured surfaces;
    // MUI's name for exactly that is primary.contrastText. The `primary`
    // group is guaranteed to exist: `hueDeltaFor` (called by `resolveScheme`
    // above) throws unless the reference pack carries BRAND_ANCHOR_TOKEN,
    // and that token lives inside it.
    (palette['primary'] as Record<string, unknown>)['contrastText'] = brand.onBrand;
  }
  return palette;
}
