import { describe, expect, it } from 'vitest';

import { formatHex, hslaToRgba, hueOf, parseColor, rgbaToHsla } from '../color';
import type { BrandInput } from '../schema';
import { BRAND_ANCHOR_TOKEN, DEFAULT_BRAND_PACK } from '../tokens';
import { catalogueFor, hueDeltaFor, resolveScheme, toMuiPalette, unflatten } from '../toMuiPalette';

const hue = (degrees: number): BrandInput => ({
  hue: formatHex(hslaToRgba({ h: degrees, s: 0.7, l: 0.5, a: 1 })),
});

const defaultBrand = DEFAULT_BRAND_PACK.brand;

describe('catalogue and hue delta', () => {
  it('takes the catalogue from the default pack, symmetrically', () => {
    const light = catalogueFor('light');
    const dark = catalogueFor('dark');
    expect(light.length).toBe(dark.length);
    expect([...light].sort()).toEqual([...dark].sort());
    expect(light).toContain(BRAND_ANCHOR_TOKEN);
  });

  it('measures the delta per scheme against that scheme’s own anchor', () => {
    const anchorDark = hueOf(DEFAULT_BRAND_PACK.schemes.dark[BRAND_ANCHOR_TOKEN] as string);
    const anchorLight = hueOf(DEFAULT_BRAND_PACK.schemes.light[BRAND_ANCHOR_TOKEN] as string);
    // Brand guide v3 puts both schemes' anchor on Base Indigo, so the
    // default pack keeps both schemes on its own hue (delta 0 in each).
    // The per-scheme anchor still matters for any pack whose two anchors
    // differ (the old cyan-dark / magenta-light split, §4.1 Blocker 1).
    expect(anchorDark).toBeCloseTo(anchorLight, 6);

    expect(hueDeltaFor('dark', defaultBrand)).toBeCloseTo(0, 6);
    expect(hueDeltaFor('light', defaultBrand)).toBeCloseTo(0, 6);
    const shifted = hue(90);
    expect(hueDeltaFor('dark', shifted)).toBeCloseTo(hueOf(shifted.hue) - anchorDark, 6);
    expect(hueDeltaFor('light', shifted)).toBeCloseTo(hueOf(shifted.hue) - anchorLight, 6);
  });

  it('lands BOTH schemes on the pack hue when a pack states only a hue', () => {
    const brand = hue(90);
    for (const scheme of ['light', 'dark'] as const) {
      const resolved = resolveScheme({}, brand, scheme);
      expect(hueOf(resolved[BRAND_ANCHOR_TOKEN] as string)).toBeCloseTo(90, 0);
    }
  });
});

describe('resolveScheme', () => {
  it('returns the reference verbatim when the pack keeps the scheme’s hue', () => {
    const resolved = resolveScheme({}, defaultBrand, 'dark');
    expect(resolved).toEqual({ ...DEFAULT_BRAND_PACK.schemes.dark });
  });

  it('prefers a stated token over the derivation', () => {
    const stated = formatHex(hslaToRgba({ h: 12, s: 0.5, l: 0.5, a: 1 }));
    const resolved = resolveScheme({ 'primary.main': stated }, hue(200), 'dark');
    expect(resolved['primary.main']).toBe(stated);
    expect(resolved['secondary.main']).not.toBe(DEFAULT_BRAND_PACK.schemes.dark['secondary.main']);
  });

  it('carries through token ids the default pack does not know', () => {
    const resolved = resolveScheme({ 'vendor.accent': 'transparent' }, defaultBrand, 'light');
    expect(resolved['vendor.accent']).toBe('transparent');
  });

  it('derives every catalogue id when the pack states none', () => {
    const resolved = resolveScheme({}, hue(90), 'light');
    expect(Object.keys(resolved).sort()).toEqual([...catalogueFor('light')].sort());
    const unchanged = Object.entries(resolved).filter(
      ([id, value]) => value === DEFAULT_BRAND_PACK.schemes.light[id],
    );
    // Only values with no parseable colour at all (keywords such as
    // `transparent`, `none`) can survive a hue change untouched.
    for (const [, value] of unchanged) expect(value).toMatch(/^(transparent|none)$/);
  });

  it('tints neutrals so a hue-only pack repaints the greys too', () => {
    const resolved = resolveScheme({}, hue(90), 'dark');
    // `step.default.border` is a mid-lightness neutral (Ink's borderStrong,
    // a cool grey). A neutral turns by the same delta as the accents.
    const sourceGrey = DEFAULT_BRAND_PACK.schemes.dark['step.default.border'] as string;
    const derivedGrey = resolved['step.default.border'] as string;
    expect(derivedGrey).not.toBe(sourceGrey);
    const expectedHue = (((hueOf(sourceGrey) + hueDeltaFor('dark', hue(90))) % 360) + 360) % 360;
    expect(rgbaToHsla(parseColor(derivedGrey) as never).h).toBeCloseTo(expectedHue, -1);

    // Pure white is repainted too, but at the saturation floor and clamped
    // lightness the chroma is ~1/255, so 8-bit rounding leaves only a few
    // degrees of hue fidelity. Being DIFFERENT is the property that matters.
    const derivedWhite = resolved['text.secondary'] as string;
    expect(derivedWhite).not.toBe(DEFAULT_BRAND_PACK.schemes.dark['text.secondary']);
  });
});

describe('unflatten', () => {
  it('expands dotted ids into nested objects', () => {
    expect(unflatten({ 'a.b.c': 'x', 'a.d': 'y' })).toEqual({ a: { b: { c: 'x' }, d: 'y' } });
  });

  it('rejects an id that nests under an existing leaf', () => {
    expect(() => unflatten({ a: 'x', 'a.b': 'y' })).toThrow(/nests under the leaf a/);
  });

  it('rejects an id that collides with an existing group', () => {
    expect(() => unflatten({ 'a.b': 'y', a: 'x' })).toThrow(/collides with a group/);
  });
});

describe('toMuiPalette', () => {
  it('sets palette.mode from the scheme name', () => {
    expect(toMuiPalette(DEFAULT_BRAND_PACK.schemes.dark, defaultBrand, 'dark').mode).toBe('dark');
    expect(toMuiPalette(DEFAULT_BRAND_PACK.schemes.light, defaultBrand, 'light').mode).toBe('light');
  });

  it('maps the optional brand.onBrand onto primary.contrastText', () => {
    const onBrand = formatHex(hslaToRgba({ h: 0, s: 0, l: 0.99, a: 1 }));
    const palette = toMuiPalette(DEFAULT_BRAND_PACK.schemes.dark, { ...defaultBrand, onBrand }, 'dark');
    expect((palette.primary as { contrastText: string }).contrastText).toBe(onBrand);
    const without = toMuiPalette(DEFAULT_BRAND_PACK.schemes.dark, defaultBrand, 'dark');
    expect((without.primary as { contrastText?: string }).contrastText).toBeUndefined();
  });
});
