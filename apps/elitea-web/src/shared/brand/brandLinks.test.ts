import { afterEach, describe, expect, it } from 'vitest';

import {
  FALLBACK_SUPPORT_EMAIL,
  docsBaseUrl,
  docsLink,
  supportEmail,
  supportUrl,
} from './brandLinks';
import { BRAND_PACK_GLOBAL } from './channelC';
import { BrandPack } from './schema';
import { DEFAULT_BRAND_PACK } from './tokens';

/** A tenant pack that states every contact field. */
const tenantPack = BrandPack.parse({
  ...DEFAULT_BRAND_PACK,
  id: 'contoso',
  product: {
    ...DEFAULT_BRAND_PACK.product,
    docsUrl: 'https://docs.contoso.example/',
    supportUrl: 'https://help.contoso.example/tickets',
    supportEmail: 'help@contoso.example',
    senderName: 'Contoso Machina',
  },
});

const globalWindow = window as unknown as Record<string, unknown>;

afterEach(() => {
  delete globalWindow[BRAND_PACK_GLOBAL];
});

describe('brand pack schema — the WP8 contact fields', () => {
  it('accepts and preserves product.supportEmail and product.senderName', () => {
    expect(tenantPack.product.supportEmail).toBe('help@contoso.example');
    expect(tenantPack.product.senderName).toBe('Contoso Machina');
  });

  it('rejects a supportEmail that is not an e-mail address', () => {
    const result = BrandPack.safeParse({
      ...DEFAULT_BRAND_PACK,
      product: { ...DEFAULT_BRAND_PACK.product, supportEmail: 'not-an-address' },
    });
    expect(result.success).toBe(false);
  });

  it('leaves both fields absent (not null) when a pack does not state them', () => {
    expect('supportEmail' in DEFAULT_BRAND_PACK.product).toBe(false);
    expect('senderName' in DEFAULT_BRAND_PACK.product).toBe(false);
  });
});

describe('brand pack schema — product.docsUrl accepts a relative or absolute URL', () => {
  it('accepts the compiled default’s root-relative /docs/ (the embedded docs SPA, issue W1b)', () => {
    const result = BrandPack.safeParse({
      ...DEFAULT_BRAND_PACK,
      product: { ...DEFAULT_BRAND_PACK.product, docsUrl: '/docs/' },
    });
    expect(result.success).toBe(true);
  });

  it('accepts an absolute override (a tenant’s externally hosted docs)', () => {
    const result = BrandPack.safeParse({
      ...DEFAULT_BRAND_PACK,
      product: { ...DEFAULT_BRAND_PACK.product, docsUrl: 'https://docs.contoso.example' },
    });
    expect(result.success).toBe(true);
  });

  it('rejects a protocol-relative //host path (it would resolve against an attacker-chosen scheme)', () => {
    const result = BrandPack.safeParse({
      ...DEFAULT_BRAND_PACK,
      product: { ...DEFAULT_BRAND_PACK.product, docsUrl: '//evil.example/docs' },
    });
    expect(result.success).toBe(false);
  });

  it('rejects a bare relative path with no leading slash', () => {
    const result = BrandPack.safeParse({
      ...DEFAULT_BRAND_PACK,
      product: { ...DEFAULT_BRAND_PACK.product, docsUrl: 'docs/' },
    });
    expect(result.success).toBe(false);
  });
});

describe('docsLink', () => {
  it('uses the given pack’s docsUrl, joining with exactly one slash', () => {
    expect(docsLink('integrations/apps/wikis', tenantPack)).toBe(
      'https://docs.contoso.example/integrations/apps/wikis',
    );
    expect(docsLink('/integrations/apps/wikis', tenantPack)).toBe(
      'https://docs.contoso.example/integrations/apps/wikis',
    );
  });

  it('returns the origin alone for an empty suffix', () => {
    expect(docsLink('', tenantPack)).toBe('https://docs.contoso.example');
    expect(docsBaseUrl(tenantPack)).toBe('https://docs.contoso.example');
  });

  it('the compiled default pack states the embedded docs SPA as a same-origin relative path', () => {
    expect(DEFAULT_BRAND_PACK.product.docsUrl).toBe('/docs/');
  });

  it('composes /docs/<path> from the default pack’s relative docsUrl', () => {
    expect(docsLink('integrations/apps/inventory', DEFAULT_BRAND_PACK)).toBe(
      '/docs/integrations/apps/inventory',
    );
    expect(docsBaseUrl(DEFAULT_BRAND_PACK)).toBe('/docs');
  });

  it('a pack that states no docsUrl falls through to the compiled default’s relative /docs/, not the literal', () => {
    // `FALLBACK_DOCS_URL` (tier 3 of the resolution order in this file's
    // header comment) is now reachable only if the compiled default pack
    // itself somehow shipped without a docsUrl, which the generator no
    // longer produces (see gen-brand-tokens.mjs PACK_META.product) — kept
    // as a defensive last resort, not exercised by this test.
    const packWithNoDocsUrl = BrandPack.parse({
      ...DEFAULT_BRAND_PACK,
      product: { name: DEFAULT_BRAND_PACK.product.name, shortName: DEFAULT_BRAND_PACK.product.shortName },
    });
    expect(packWithNoDocsUrl.product.docsUrl).toBeUndefined();
    expect(docsLink('integrations/apps/inventory', packWithNoDocsUrl)).toBe(
      '/docs/integrations/apps/inventory',
    );
  });

  it('still honours an absolute docsUrl override from a served (tenant) brand pack over the relative default', () => {
    expect(docsLink('integrations/apps/wikis', tenantPack)).toBe(
      'https://docs.contoso.example/integrations/apps/wikis',
    );
  });

  it('reads the served pack from window when no pack is given, else the compiled default’s relative /docs/', () => {
    globalWindow[BRAND_PACK_GLOBAL] = tenantPack;
    expect(docsLink('x')).toBe('https://docs.contoso.example/x');

    delete globalWindow[BRAND_PACK_GLOBAL];
    expect(docsLink('x')).toBe('/docs/x');
  });
});

describe('supportEmail / supportUrl', () => {
  it('prefer the pack’s own values', () => {
    expect(supportEmail(tenantPack)).toBe('help@contoso.example');
    expect(supportUrl(tenantPack)).toBe('https://help.contoso.example/tickets');
  });

  it('fall back to the shipped address, and to a mailto: of it for the URL', () => {
    expect(supportEmail(DEFAULT_BRAND_PACK)).toBe(FALLBACK_SUPPORT_EMAIL);
    expect(supportUrl(DEFAULT_BRAND_PACK)).toBe(`mailto:${FALLBACK_SUPPORT_EMAIL}`);
  });

  it('build the mailto: from the pack’s e-mail when it states an e-mail but no URL', () => {
    const emailOnly = BrandPack.parse({
      ...DEFAULT_BRAND_PACK,
      product: { ...DEFAULT_BRAND_PACK.product, supportEmail: 'help@contoso.example' },
    });
    expect(supportUrl(emailOnly)).toBe('mailto:help@contoso.example');
  });

  it('read the served pack from window when no pack is given', () => {
    globalWindow[BRAND_PACK_GLOBAL] = tenantPack;
    expect(supportEmail()).toBe('help@contoso.example');
    expect(supportUrl()).toBe('https://help.contoso.example/tickets');
  });
});
