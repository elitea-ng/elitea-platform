import { afterEach, describe, expect, it, vi } from 'vitest';

import { BRAND_PACK_GLOBAL } from './channelC';
import { productFullName, productName } from './productName';
import { BrandPack } from './schema';
import { DEFAULT_BRAND_PACK } from './tokens';

const servedPack = BrandPack.parse({
  ...DEFAULT_BRAND_PACK,
  id: 'contoso',
  product: { ...DEFAULT_BRAND_PACK.product, name: 'Contoso Machina', shortName: 'Machina' },
});

const globalWindow = window as unknown as Record<string, unknown>;

afterEach(() => {
  delete globalWindow[BRAND_PACK_GLOBAL];
  vi.resetModules();
});

describe('productName (ADR-0052 W3.2)', () => {
  it('is the compiled default pack’s with no served pack', () => {
    expect(productName()).toBe(DEFAULT_BRAND_PACK.product.shortName);
    expect(productFullName()).toBe(DEFAULT_BRAND_PACK.product.name);
  });

  it('follows the served pack', () => {
    globalWindow[BRAND_PACK_GLOBAL] = servedPack;
    expect(productName()).toBe('Machina');
    expect(productFullName()).toBe('Contoso Machina');
  });

  it('ignores an invalid served pack', () => {
    globalWindow[BRAND_PACK_GLOBAL] = { product: { shortName: 'Broken' } };
    vi.spyOn(console, 'warn').mockImplementation(() => {});
    expect(productName()).toBe(DEFAULT_BRAND_PACK.product.shortName);
  });

  it('resolves a served pack once, and a new served pack afresh', () => {
    globalWindow[BRAND_PACK_GLOBAL] = servedPack;
    expect(productName()).toBe('Machina');
    globalWindow[BRAND_PACK_GLOBAL] = { ...servedPack, product: { ...servedPack.product, shortName: 'Other' } };
    expect(productName()).toBe('Other');
  });

  it('applyProductName re-reads the name a later-published pack states (the desktop launch)', async () => {
    const { applyProductName, t } = await import('@/shared/i18n');
    globalWindow[BRAND_PACK_GLOBAL] = servedPack;
    applyProductName();
    expect(t('pages.admin.nav.backToApp', 'Back to {{product}}')).toBe('Back to Machina');
    delete globalWindow[BRAND_PACK_GLOBAL];
    applyProductName();
    expect(t('pages.admin.nav.backToApp', 'Back to {{product}}')).toBe(`Back to ${DEFAULT_BRAND_PACK.product.shortName}`);
  });

  it('fills {{product}} in every message from the pack loaded before the bundle', async () => {
    globalWindow[BRAND_PACK_GLOBAL] = servedPack;
    vi.resetModules();
    const { t } = await import('@/shared/i18n');
    expect(t('pages.admin.nav.backToApp', 'Back to {{product}}')).toBe('Back to Machina');
    expect(t('entries.maintenance.page.headline', '{{product}} is under maintenance!')).toBe(
      'Machina is under maintenance!',
    );
  });
});
