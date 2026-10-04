import { describe, expect, it } from 'vitest';

import { BrandPack } from '../schema';
import { BRAND_ANCHOR_TOKEN, DEFAULT_BRAND_PACK } from '../tokens';

/**
 * Invariants of the GENERATED default pack. These are the properties the
 * conversion script promises; asserting them here means a regeneration that
 * loses one fails the suite instead of quietly shipping.
 */
describe('the default pack', () => {
  it('is schema-valid and identifies itself as the default', () => {
    expect(() => BrandPack.parse(DEFAULT_BRAND_PACK)).not.toThrow();
    expect(DEFAULT_BRAND_PACK.id).toBe('default');
    expect(DEFAULT_BRAND_PACK.product.name).toBe('Elitea');
  });

  it('carries symmetric scheme records', () => {
    const light = Object.keys(DEFAULT_BRAND_PACK.schemes.light).sort();
    const dark = Object.keys(DEFAULT_BRAND_PACK.schemes.dark).sort();
    expect(light).toEqual(dark);
    // 406 at baseline 20b23c42 (was 362 at a55f36cf): the baseline added 44
    // tokens, and both new asymmetric ones carry a SYMMETRY_FILLS entry, which
    // is why the light/dark equality above still holds. 407 with #6687's
    // `border.artifactDivider` (an ADDITIONS row in gen-brand-tokens.mjs).
    expect(light.length).toBe(407);
  });

  it('states the mandatory §4.2 roles in both schemes', () => {
    for (const scheme of ['light', 'dark'] as const) {
      const record = DEFAULT_BRAND_PACK.schemes[scheme];
      for (const id of [
        'error.main',
        'error.light',
        'error.dark',
        'error.contrastText',
        'success.main',
        'success.dark',
        'success.contrastText',
      ]) {
        expect(record[id], `${scheme}.${id}`).toBeDefined();
      }
    }
  });

  it('states the brand-accent tokens that replaced the BaseBtn branches', () => {
    for (const scheme of ['light', 'dark'] as const) {
      const record = DEFAULT_BRAND_PACK.schemes[scheme];
      for (const id of [
        'background.button.special.default',
        'background.button.special.hover',
        'background.button.special.pressed',
        'background.button.maxi.default',
        'background.button.maxi.hover',
        'background.button.maxi.pressed',
        'background.button.iconCounter.pressed',
        'text.button.specialDefault',
        'text.button.specialPressed',
        'text.button.maxiDefault',
        'text.button.secondaryPressed',
        'text.button.auxiliaryDefault',
        'text.button.auxiliaryHover',
        'text.button.auxiliaryPressed',
      ]) {
        expect(record[id], `${scheme}.${id}`).toBeDefined();
      }
    }
  });

  it('does NOT carry the tokens unit T2 classified as bugs', () => {
    for (const scheme of ['light', 'dark'] as const) {
      const record = DEFAULT_BRAND_PACK.schemes[scheme];
      // T2 §1.4/§1.16 — moderation space, removed by elitea-ui d46cb93.
      expect(record['background.moderator']).toBeUndefined();
      // T2 §3 — phantom reference in admin-ui's MuiRadio/MuiCheckbox.
      expect(record['icon.fill.select']).toBeUndefined();
      // `mode` is scheme identity, not a token.
      expect(record['mode']).toBeUndefined();
    }
  });

  it('anchors the derivation on a token both schemes state', () => {
    expect(DEFAULT_BRAND_PACK.schemes.light[BRAND_ANCHOR_TOKEN]).toBeDefined();
    expect(DEFAULT_BRAND_PACK.schemes.dark[BRAND_ANCHOR_TOKEN]).toBeDefined();
    expect(DEFAULT_BRAND_PACK.brand.hue).toBe(DEFAULT_BRAND_PACK.schemes.dark[BRAND_ANCHOR_TOKEN]);
  });
});
