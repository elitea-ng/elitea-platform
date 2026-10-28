/**
 * `@font-face` sources rewritten to object URLs (ADR-0024 WP9).
 */
import { describe, expect, it } from 'vitest';

import { DEFAULT_BRAND_PACK, fontFaceStylesheet, type BrandPack } from '@/shared/brand';

import { targetId, type LoadedAsset } from '../assets';
import { previewFontStylesheet, rewriteFontFaceSources } from '../fontSources';

const pack: BrandPack = {
  ...DEFAULT_BRAND_PACK,
  typography: {
    ...DEFAULT_BRAND_PACK.typography,
    fontFaces: [
      { family: 'Tenant Sans', url: './fonts/TenantSans-Regular.woff2', weight: '400' },
      { family: 'Tenant Sans', url: './fonts/TenantSans-Bold.woff2', weight: '700', style: 'italic' },
    ],
  },
};

describe('rewriteFontFaceSources', () => {
  it('replaces only the sources the resolver knows', () => {
    const css = fontFaceStylesheet(pack);
    const out = rewriteFontFaceSources(css, (url) => (url.endsWith('Bold.woff2') ? 'blob:null/bold' : undefined));
    expect(out).toContain('src:url("blob:null/bold") format("woff2")');
    expect(out).toContain('src:url("./fonts/TenantSans-Regular.woff2") format("woff2")');
    expect(out).not.toContain('./fonts/TenantSans-Bold.woff2');
  });

  it('keeps every other declaration byte for byte', () => {
    const css = fontFaceStylesheet(pack);
    const out = rewriteFontFaceSources(css, () => 'blob:null/x');
    expect(out.replaceAll('blob:null/x', '')).toBe(css.replaceAll('./fonts/TenantSans-Regular.woff2', '').replaceAll('./fonts/TenantSans-Bold.woff2', ''));
    expect(out).toContain('font-weight:700');
    expect(out).toContain('font-style:italic');
    expect(out).toContain('font-display:swap');
  });

  it('is the identity on a stylesheet with no sources', () => {
    expect(rewriteFontFaceSources('', () => 'blob:null/x')).toBe('');
    expect(rewriteFontFaceSources('body{margin:0}', () => 'blob:null/x')).toBe('body{margin:0}');
  });
});

describe('previewFontStylesheet', () => {
  it('sources a loaded font from memory and leaves the rest on their pack paths', () => {
    const bold: LoadedAsset = {
      fileName: 'TenantSans-Bold.woff2',
      objectUrl: 'blob:null/bold',
      target: { kind: 'font', fileName: 'TenantSans-Bold.woff2' },
    };
    const css = previewFontStylesheet(pack, new Map([[targetId(bold.target), bold]]));
    expect(css.match(/@font-face/g)).toHaveLength(2);
    expect(css).toContain('url("blob:null/bold")');
    expect(css).toContain('url("./fonts/TenantSans-Regular.woff2")');
  });

  it('is empty for a pack that declares no face', () => {
    const { fontFaces: _omitted, ...typography } = DEFAULT_BRAND_PACK.typography;
    expect(previewFontStylesheet({ ...DEFAULT_BRAND_PACK, typography }, new Map())).toBe('');
  });
});
