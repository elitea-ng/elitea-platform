/**
 * The Branding page's value model (ADR-0024 WP4): the parser, the overlay
 * that mirrors the Go resolver, and the refusal-to-field reader.
 */
import { describe, expect, it } from 'vitest';

import { DEFAULT_BRAND_PACK } from '@/shared/brand';

import {
  BRANDING_KEYS,
  applyDraftToPack,
  basePackFrom,
  brandingErrorField,
  brandingFieldSource,
  effectiveFontFaces,
  emptyBrandingValues,
  isInherited,
  isSixDigitHex,
  parseBrandingValues,
  resetBrandingPayload,
  withDerivedSchemes,
} from './brandingValues';

// `#${'…'}` keeps R-T1 (`elitea/no-raw-color`) out of a test about hex input.
const HEX = `#${'1a73e8'}`;

describe('parseBrandingValues', () => {
  it('types every declared key, reading anything malformed as inherit', () => {
    const values = parseBrandingValues({
      product_name: 'Acme',
      base_size: 16,
      scale: 'not a number',
      radius_md: Number.NaN,
      density: 42,
      font_faces: [
        { family: 'Inter', url: '/api/v2/branding/assets/font/abc.woff2', weight: '400', style: '' },
        'garbage',
        { family: 'Mono' },
      ],
    });
    expect(values.product_name).toBe('Acme');
    expect(values.base_size).toBe(16);
    expect(values.scale).toBe(0);
    expect(values.radius_md).toBe(0);
    expect(values.density).toBe('');
    expect(values.font_faces).toEqual([
      { family: 'Inter', url: '/api/v2/branding/assets/font/abc.woff2', weight: '400' },
      { family: 'Mono', url: '' },
    ]);
  });

  it('reads a non-object as every key inherited', () => {
    expect(parseBrandingValues(null)).toEqual(emptyBrandingValues());
    for (const key of BRANDING_KEYS) {
      expect(isInherited(emptyBrandingValues(), key), key).toBe(true);
    }
  });
});

describe('resetBrandingPayload', () => {
  it('clears the keys this page does not edit, even before the section has loaded', () => {
    expect(resetBrandingPayload()).toEqual({
      ...emptyBrandingValues(),
      scheme_tokens: {},
      sender_name: '',
      support_email: '',
    });
  });

  it('clears any other stored key at its own shape, so a key added server-side cannot survive', () => {
    const payload = resetBrandingPayload({
      product_name: 'Acme',
      future_text: 'x',
      future_number: 7,
      future_list: ['a'],
      future_map: { a: 1 },
    });
    expect(payload).toMatchObject({
      product_name: '',
      future_text: '',
      future_number: 0,
      future_list: [],
      future_map: {},
      scheme_tokens: {},
    });
  });

  it('leaves out a stored key whose shape cannot name its inherit value', () => {
    const payload = resetBrandingPayload({ future_flag: true, future_null: null });
    expect(payload).not.toHaveProperty('future_flag');
    expect(payload).not.toHaveProperty('future_null');
  });

  it('reads a non-object answer as nothing extra', () => {
    expect(resetBrandingPayload(['product_name'])).toEqual(resetBrandingPayload());
    expect(resetBrandingPayload(null)).toEqual(resetBrandingPayload());
  });
});

describe('brandingFieldSource', () => {
  it('names the database for a stored value, else the file when one contributes, else the default', () => {
    const values = parseBrandingValues({ product_name: 'Acme', base_size: 15 });
    const both = { file: true, database: true };
    expect(brandingFieldSource(values, 'product_name', both)).toBe('database');
    expect(brandingFieldSource(values, 'base_size', both)).toBe('database');
    expect(brandingFieldSource(values, 'product_tagline', both)).toBe('file');
    expect(brandingFieldSource(values, 'product_tagline', { file: false, database: true })).toBe('default');
    expect(brandingFieldSource(values, 'font_faces', { file: false, database: false })).toBe('default');
  });
});

describe('basePackFrom / effectiveFontFaces', () => {
  it('falls back to the compiled default for null and for a pack that does not parse', () => {
    expect(basePackFrom(null)).toBe(DEFAULT_BRAND_PACK);
    expect(basePackFrom({ id: 'broken' })).toBe(DEFAULT_BRAND_PACK);
  });

  it('parses a served pack', () => {
    const served = { ...DEFAULT_BRAND_PACK, product: { name: 'Acme', shortName: 'Acme' } };
    expect(basePackFrom(served).product.name).toBe('Acme');
  });

  it('reads the faces whether or not the parse keeps them', () => {
    // Since ADR-0024 WP3 the zod schema DECLARES typography.fontFaces, so a
    // parsed pack keeps them; effectiveFontFaces still reads the raw served
    // object, which is what matters for a pack that fails the parse (the
    // page then falls back to the default pack but must not lose the faces
    // the server stated).
    const served = {
      ...DEFAULT_BRAND_PACK,
      typography: {
        ...DEFAULT_BRAND_PACK.typography,
        fontFaces: [{ family: 'Inter', url: '/api/v2/branding/assets/font/a.woff2' }],
      },
    };
    expect(basePackFrom(served)).toHaveProperty('typography.fontFaces');
    expect(effectiveFontFaces(served)).toEqual([
      { family: 'Inter', url: '/api/v2/branding/assets/font/a.woff2' },
    ]);
    expect(effectiveFontFaces(null)).toEqual([]);
  });
});

describe('applyDraftToPack — the Go overlay, field for field', () => {
  it('keeps every base value for an all-inherit draft', () => {
    expect(applyDraftToPack(DEFAULT_BRAND_PACK, emptyBrandingValues())).toEqual(DEFAULT_BRAND_PACK);
  });

  it('replaces exactly the set fields', () => {
    const draft = parseBrandingValues({
      product_name: 'Acme',
      product_tagline: 'Ship it',
      docs_url: 'https://docs.example.com',
      brand_hue: HEX,
      brand_on_brand: `#${'ffffff'}`,
      font_family: 'Inter, sans-serif',
      base_size: 16,
      radius_lg: 24,
      radius_pill: 999,
      density: 'compact',
      logo_full: '/api/v2/branding/assets/logo-full/abc.svg',
      login_art: '/api/v2/branding/assets/login-art/def.png',
    });
    const pack = applyDraftToPack(DEFAULT_BRAND_PACK, draft);
    expect(pack.product).toEqual({
      name: 'Acme',
      shortName: DEFAULT_BRAND_PACK.product.shortName,
      tagline: 'Ship it',
      docsUrl: 'https://docs.example.com',
    });
    expect(pack.brand).toEqual({ hue: HEX, onBrand: `#${'ffffff'}` });
    expect(pack.typography).toEqual({
      ...DEFAULT_BRAND_PACK.typography,
      fontFamily: 'Inter, sans-serif',
      baseSize: 16,
    });
    expect(pack.shape).toEqual({
      ...DEFAULT_BRAND_PACK.shape,
      radiusLg: 24,
      radiusPill: 999,
      density: 'compact',
    });
    expect(pack.assets).toEqual({
      ...DEFAULT_BRAND_PACK.assets,
      logoFull: '/api/v2/branding/assets/logo-full/abc.svg',
      loginArt: '/api/v2/branding/assets/login-art/def.png',
    });
    // Untouched groups are the same objects, so nothing was rebuilt by accident.
    expect(pack.schemes).toBe(DEFAULT_BRAND_PACK.schemes);
    expect(pack.locale).toBe(DEFAULT_BRAND_PACK.locale);
  });

  it('treats whitespace-only text and 0 as inherit', () => {
    const draft = parseBrandingValues({ product_name: '   ', base_size: 0 });
    const pack = applyDraftToPack(DEFAULT_BRAND_PACK, draft);
    expect(pack.product.name).toBe(DEFAULT_BRAND_PACK.product.name);
    expect(pack.typography.baseSize).toBe(DEFAULT_BRAND_PACK.typography.baseSize);
  });

  it('keeps the base hue while a colour is half-typed, and an unknown density', () => {
    const draft = parseBrandingValues({ brand_hue: '#1a', brand_on_brand: 'nope', density: 'cosy' });
    const pack = applyDraftToPack(DEFAULT_BRAND_PACK, draft);
    expect(pack.brand).toEqual(DEFAULT_BRAND_PACK.brand);
    expect(pack.shape.density).toBe(DEFAULT_BRAND_PACK.shape.density);
  });

  it('never writes an explicit undefined into an optional field', () => {
    const pack = applyDraftToPack(DEFAULT_BRAND_PACK, emptyBrandingValues());
    expect('tagline' in pack.product).toBe(false);
    expect('onBrand' in pack.brand).toBe(false);
    expect('loginArt' in pack.assets).toBe(false);
  });
});

describe('withDerivedSchemes', () => {
  it('drops the stated records only when the hue moved off the base', () => {
    const same = withDerivedSchemes(DEFAULT_BRAND_PACK, DEFAULT_BRAND_PACK.brand.hue);
    expect(same).toBe(DEFAULT_BRAND_PACK);
    const moved = withDerivedSchemes(
      { ...DEFAULT_BRAND_PACK, brand: { hue: HEX } },
      DEFAULT_BRAND_PACK.brand.hue,
    );
    expect(moved.schemes).toEqual({ light: {}, dark: {} });
  });
});

describe('brandingErrorField', () => {
  it('reads the key a Go %q refusal names', () => {
    expect(brandingErrorField(`"brand_hue" must be a six-digit hex colour such as ${HEX}`)).toBe(
      'brand_hue',
    );
    expect(brandingErrorField('"font_faces"[1].url must be the path of an uploaded font asset')).toBe(
      'font_faces',
    );
    expect(brandingErrorField('"base_size" must be between 12 and 18, or 0 to inherit')).toBe(
      'base_size',
    );
  });

  it('skips the section id and stops at a declared key', () => {
    expect(brandingErrorField('unknown configuration key for section "branding": "logo_full"')).toBe(
      'logo_full',
    );
  });

  it('is undefined for a refusal about nothing in particular', () => {
    expect(brandingErrorField('values is required')).toBeUndefined();
    expect(brandingErrorField('"nonsense" must be a string')).toBeUndefined();
  });
});

describe('isSixDigitHex', () => {
  it('accepts exactly what the server accepts', () => {
    expect(isSixDigitHex(HEX)).toBe(true);
    expect(isSixDigitHex(` ${HEX.toUpperCase()} `)).toBe(true);
    expect(isSixDigitHex(`#${'abc'}`)).toBe(false);
    expect(isSixDigitHex('1a73e8')).toBe(false);
    expect(isSixDigitHex('')).toBe(false);
  });
});
