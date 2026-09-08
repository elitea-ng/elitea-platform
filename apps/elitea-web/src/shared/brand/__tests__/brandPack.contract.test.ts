import { resolve } from 'node:path';

import { createTheme } from '@mui/material/styles';
import { describe, expect, it } from 'vitest';

import { formatHex, hslaToRgba } from '../color';
import { CSS_VAR_PREFIX } from '../constants';
import { buildEliteaTheme } from '../buildTheme';
import { BrandPack } from '../schema';
import { DEFAULT_BRAND_PACK } from '../tokens';
import {
  colorsIn,
  colorsInTheme,
  emittedCssVars,
  sampleRenderedColors,
  scanThemeVarReferences,
} from './reference-scan';
import { renderAllSurfaces } from './render-surfaces';
import { COVERED_OVERRIDE_KEYS, WIRED_OVERRIDE_KEYS } from './surfaces';

/**
 * §4.6 check 7 — the brand-pack round trip. `npm run theme-gate` runs exactly
 * this file as its seventh, self-arming check.
 *
 * The four assertions below are the spec's four, in order and in full.
 */

const SRC_ROOT = resolve(import.meta.dirname, '../../..');

/**
 * The hostile pack: a different hue, compact density, zero radii, different
 * fonts, a different product name — and NO scheme records at all.
 *
 * Empty records are the strongest available form of the spec's "different
 * hue" requirement: with nothing stated, every one of the 362 token ids has
 * to come out of the hue derivation, so the assertion measures the
 * derivation rather than a hand-written second palette.
 *
 * The hue is computed rather than written, both because a colour literal in
 * this file would (correctly) fail R-T1, and because stating it in degrees
 * makes the distance from the two baseline anchors explicit: 90° against
 * cyan's ~187° and magenta's ~291°.
 */
const HOSTILE_HUE_DEGREES = 90;
const hostilePack = BrandPack.parse({
  ...DEFAULT_BRAND_PACK,
  id: 'hostile',
  version: '9.9.9',
  product: { name: 'Contoso Machina', shortName: 'CM' },
  typography: {
    fontFamily: 'Georgia, "Times New Roman", serif',
    fontFamilyMono: 'Consolas, monospace',
    baseSize: 18,
    scale: 1.5,
  },
  shape: { radiusSm: 0, radiusMd: 0, radiusLg: 0, radiusPill: 0, density: 'compact' },
  brand: { hue: formatHex(hslaToRgba({ h: HOSTILE_HUE_DEGREES, s: 0.72, l: 0.5, a: 1 })) },
  schemes: { light: {}, dark: {} },
});

const SCHEMES = ['light', 'dark'] as const;
const ROLES = ['error', 'success'] as const;

type ThemeLike = ReturnType<typeof createTheme>;

const roleOf = (
  theme: ThemeLike,
  scheme: (typeof SCHEMES)[number],
  role: (typeof ROLES)[number],
): Record<string, string> => {
  const colorScheme = theme.colorSchemes[scheme];
  if (colorScheme === undefined) throw new Error(`theme carries no ${scheme} colour scheme`);
  // PaletteColor's slots are a closed interface; the test indexes them by
  // name, which is the one place a widening cast is simpler than four cases.
  return colorScheme.palette[role] as unknown as Record<string, string>;
};

/**
 * Every slot the default pack STATES for a role must arrive verbatim, and
 * must differ from what MUI would have supplied on its own. `success.light`
 * is deliberately not stated — the baseline ramp has no lighter green — so it
 * is checked separately: derived by MUI's augmentColor FROM the pack's main,
 * hence still not MUI's default.
 */
function assertRoleComesFromThePack(
  theme: ThemeLike,
  fallbackTheme: ThemeLike,
  scheme: (typeof SCHEMES)[number],
  role: (typeof ROLES)[number],
): void {
  const actual = roleOf(theme, scheme, role);
  const fallback = roleOf(fallbackTheme, scheme, role);
  const packed = DEFAULT_BRAND_PACK.schemes[scheme];
  let stated = 0;
  for (const slot of ['main', 'light', 'dark', 'contrastText']) {
    const expected = packed[`${role}.${slot}`];
    if (expected === undefined) {
      expect(actual[slot], `${scheme}.${role}.${slot} is derived, not MUI's`).not.toBe(
        fallback[slot],
      );
      continue;
    }
    stated += 1;
    expect(actual[slot], `${scheme}.${role}.${slot}`).toBe(expected);
    expect(actual[slot], `${scheme}.${role}.${slot} differs from MUI's default`).not.toBe(
      fallback[slot],
    );
  }
  expect(stated, `${scheme}.${role} states at least main/dark/contrastText`).toBeGreaterThanOrEqual(3);
}

describe('§4.6 check 7 — brand-pack round trip', () => {
  it('(a) parses the default pack and rejects an unknown key (.strict)', () => {
    expect(() => BrandPack.parse(DEFAULT_BRAND_PACK)).not.toThrow();

    const withUnknownKey = { ...DEFAULT_BRAND_PACK, tenantId: 'acme' };
    const result = BrandPack.safeParse(withUnknownKey);
    expect(result.success).toBe(false);
    expect(JSON.stringify(result.error?.issues)).toContain('tenantId');

    // The strictness is TOP-LEVEL only — nested unknown keys are stripped,
    // which is the behaviour unit W3's Go mirror implements. Asserting it
    // here keeps the two implementations from drifting apart silently.
    const nestedUnknown = BrandPack.parse({
      ...DEFAULT_BRAND_PACK,
      product: { ...DEFAULT_BRAND_PACK.product, nickname: 'El' },
    });
    expect(nestedUnknown.product).not.toHaveProperty('nickname');
  });

  it('(a′) accepts typography.fontFaces (ADR-0024 WP3) with exactly the mirrored shape, and stays strict', () => {
    const withFaces = BrandPack.parse({
      ...DEFAULT_BRAND_PACK,
      typography: {
        ...DEFAULT_BRAND_PACK.typography,
        fontFaces: [
          { family: 'Montserrat', url: '/api/v2/branding/assets/font/abc.woff2', weight: '400', style: 'normal' },
          { family: 'Montserrat', url: '/api/v2/branding/assets/font/def.woff2' },
        ],
      },
    });
    expect(withFaces.typography.fontFaces).toHaveLength(2);
    expect(withFaces.typography.fontFaces?.[1]).toEqual({
      family: 'Montserrat',
      url: '/api/v2/branding/assets/font/def.woff2',
    });

    // The default pack declares none; an absent array is absent, not [].
    expect(DEFAULT_BRAND_PACK.typography.fontFaces).toBeUndefined();

    // Field-level contract the Go mirror shares: family/url non-empty, style
    // is a closed enum.
    for (const bad of [
      { family: '', url: '/a.woff2' },
      { family: 'M', url: '' },
      { family: 'M', url: '/a.woff2', style: 'oblique' },
    ]) {
      const result = BrandPack.safeParse({
        ...DEFAULT_BRAND_PACK,
        typography: { ...DEFAULT_BRAND_PACK.typography, fontFaces: [bad] },
      });
      expect(result.success, JSON.stringify(bad)).toBe(false);
    }

    // Top-level strictness is unchanged by the addition.
    expect(BrandPack.safeParse({ ...withFaces, fontFaces: [] }).success).toBe(false);
  });

  it('(b) emits every --el-palette-* variable the source tree references', () => {
    const references = scanThemeVarReferences(SRC_ROOT);
    expect(references.length).toBeGreaterThan(0);

    const emitted = emittedCssVars(buildEliteaTheme(DEFAULT_BRAND_PACK));
    const missing = references.filter((ref) => !emitted.has(ref.cssVar));
    expect(
      missing.map((ref) => `${ref.file}:${ref.line} ${ref.cssVar}`),
      'referenced-but-undefined palette tokens',
    ).toEqual([]);

    // The reverse direction is deliberately NOT asserted: an unreferenced
    // token is not an error (spec §4.6 check 7.2), it is a token nothing has
    // been authored against yet — this headroom check only confirms that
    // is still true (some token remains unconsumed), so it must count
    // DISTINCT referenced tokens, not raw reference occurrences: a real app
    // legitimately re-reads the same handful of tokens from hundreds of
    // call sites (934 occurrences of 174 distinct tokens, as of Wave 2
    // batch 2), and comparing occurrence count against `emitted.size` would
    // start failing purely from adding more pages, with no loss of headroom
    // at all.
    const uniqueReferencedTokens = new Set(references.map((ref) => ref.cssVar));
    expect(emitted.size).toBeGreaterThan(uniqueReferencedTokens.size);
    for (const ref of references) {
      expect(ref.cssVar.startsWith(`--${CSS_VAR_PREFIX}-palette-`)).toBe(true);
    }
    // [F2] explicit timeout, not the 5000ms default: `scanThemeVarReferences`
    // reads every source file under `src/` (4294 as of #806) and Babel-parses
    // the ones that can carry a reference — legitimately slower than 5s under
    // CI's shared runners when this test lands near the tail of the full,
    // highly-parallel suite run (observed: PR #21's first batch-2 run, real
    // timeout at 5000ms with 813/815 other files already passed).
    //
    // The budget stays 20s, but the scan no longer needs most of it. It
    // exceeded even 20s once (PR #806, shard 2 of 5 of the coverage run),
    // because the cost is paid TWICE over under `--coverage`: the walk of
    // every file's AST runs through instrumented statements. Measured here,
    // same machine, this file under `--coverage`: 7.12s of test time before
    // the scan learned to skip files that cannot contribute, 2.07s after
    // (`reference-scan.ts`'s CANDIDATE_RE — ~90% of the tree is skipped, and
    // the results are identical). Without coverage it is ~2.2s and ~0.9s.
  }, 20_000);

  it('(c) renders the available surface under a hostile pack with zero default-pack colours', () => {
    // Colours the default pack states, minus the ones a bare MUI theme also
    // emits: `#fff`, MUI's grey ramp and friends appear in ANY theme, so they
    // cannot discriminate a repaint and would only produce a false failure.
    const baselineTheme = createTheme({
      cssVariables: { cssVarPrefix: CSS_VAR_PREFIX },
      colorSchemes: { light: true, dark: true },
    });
    const nonDiscriminating = colorsInTheme(baselineTheme);

    // A FULLY TRANSPARENT colour is non-discriminating for the same reason the
    // bare-MUI palette above is: alpha 0 paints nothing, so it appears in
    // rendered output no matter which pack is mounted and cannot evidence a
    // repaint. `background.folder.shadow` carries one — the dark half of a
    // light-only baseline token, where the exact rendering equivalent of "no
    // drop shadow" is a transparent zero-offset shadow (see SYMMETRY_FILLS in
    // scripts/gen-brand-tokens.mjs). Counting it produced a failure that named
    // a leak but described a no-op.
    const isFullyTransparent = (color: string) => /^#[0-9a-f]{6}00$/i.test(color);

    const defaultPackColors = new Set<string>();
    for (const record of [DEFAULT_BRAND_PACK.schemes.light, DEFAULT_BRAND_PACK.schemes.dark]) {
      for (const value of Object.values(record)) {
        colorsIn(value).forEach((color) => {
          if (!nonDiscriminating.has(color) && !isFullyTransparent(color)) defaultPackColors.add(color);
        });
      }
    }
    expect(defaultPackColors.size).toBeGreaterThan(100);

    // The sweep must cover every override key that is actually wired, so it
    // widens automatically as unit S1 fills in the remaining ~28 keys.
    expect(COVERED_OVERRIDE_KEYS.sort()).toEqual([...WIRED_OVERRIDE_KEYS].sort());

    const hostileTheme = buildEliteaTheme(hostilePack);
    const { document, unmount } = renderAllSurfaces(hostileTheme);
    try {
      const sample = sampleRenderedColors(document, 200);
      // Honest scope: the route tree does not exist yet, so the sample is
      // "every element currently renderable", capped at the spec's 200 — not
      // 200 elements of a full application.
      expect(sample.size).toBeGreaterThan(0);
      const leaked = [...sample.colors].filter((color) => defaultPackColors.has(color));
      expect(leaked, 'default-pack colours surviving a hostile pack').toEqual([]);
    } finally {
      unmount();
    }

    // Same assertion at the variable layer, which is where the colours the
    // components reference actually resolve (jsdom does not resolve var()).
    const themeColors = colorsInTheme(hostileTheme);
    expect([...themeColors].filter((color) => defaultPackColors.has(color))).toEqual([]);
  });

  it('(d) resolves palette.error and palette.success to pack values, not MUI defaults', () => {
    const muiDefaults = createTheme({ colorSchemes: { light: true, dark: true } });
    const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

    for (const scheme of SCHEMES) {
      for (const role of ROLES) {
        assertRoleComesFromThePack(theme, muiDefaults, scheme, role);
      }
    }

    // A hostile pack moves them too — they are ordinary tokens, so they take
    // part in the derivation like everything else.
    const hostile = buildEliteaTheme(hostilePack);
    for (const scheme of SCHEMES) {
      for (const role of ROLES) {
        expect(roleOf(hostile, scheme, role).main).not.toBe(roleOf(theme, scheme, role).main);
      }
    }
  });
});
