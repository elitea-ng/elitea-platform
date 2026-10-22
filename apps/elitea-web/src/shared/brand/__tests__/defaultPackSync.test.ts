import { describe, expect, it } from 'vitest';

import { DEFAULT_BRAND_HUE, DEFAULT_PACK_ACCENTS, DEFAULT_PRODUCT } from '@elitea/client-core/brand';

import { DEFAULT_BRAND_PACK } from '../tokens';

/**
 * The default pack has three hand-maintained copies since gen-brand-tokens.mjs
 * went with the apps/elitea-ui submodule whose palettes it read:
 * default.pack.json (here), client-core's identity.ts and its
 * defaultPack.gen.ts (the mobile app's accent inputs). The generator's
 * `--check` used to hold them together; this test does now.
 */
describe('default brand pack copies agree', () => {
  it('takes its product name and hue from the client-core identity', () => {
    expect(DEFAULT_BRAND_PACK.product.name).toBe(DEFAULT_PRODUCT.name);
    expect(DEFAULT_BRAND_PACK.product.shortName).toBe(DEFAULT_PRODUCT.shortName);
    expect(DEFAULT_BRAND_PACK.brand.hue).toBe(DEFAULT_BRAND_HUE);
  });

  it('carries the same accents as client-core DEFAULT_PACK_ACCENTS', () => {
    expect(DEFAULT_PACK_ACCENTS.hue).toBe(DEFAULT_BRAND_PACK.brand.hue);
    for (const scheme of ['light', 'dark'] as const) {
      const record = DEFAULT_BRAND_PACK.schemes[scheme];
      expect(DEFAULT_PACK_ACCENTS[scheme].accent).toBe(record['primary.main']);
      expect(DEFAULT_PACK_ACCENTS[scheme].onAccent).toBe(record['text.button.primary']);
    }
  });
});
