import { afterEach, describe, expect, it, vi } from 'vitest';

import { fontFaceRule, fontFaceStylesheet, isSameOriginAssetPath, resolveFontUrl } from './fontFaces';
import { DEFAULT_BRAND_PACK } from './tokens';

afterEach(() => {
  vi.restoreAllMocks();
});

describe('isSameOriginAssetPath', () => {
  it('accepts root-relative and document-relative paths', () => {
    expect(isSameOriginAssetPath('/api/v2/branding/assets/font/abc.woff2')).toBe(true);
    expect(isSameOriginAssetPath('./fonts/a.woff2')).toBe(true);
  });

  it('rejects every foreign or url()-breaking form', () => {
    for (const url of [
      'https://fonts.example.com/s/montserrat.woff2',
      '//cdn.example.com/a.woff2',
      'data:font/woff2;base64,AAAA',
      'fonts/a.woff2',
      '/a b.woff2',
      '/a").woff2',
      "/a'.woff2",
      '/a)x.woff2',
      '',
    ]) {
      expect(isSameOriginAssetPath(url), url).toBe(false);
    }
  });
});

describe('fontFaceRule', () => {
  it('emits family, woff2 src, swap, and the optional weight/style', () => {
    expect(
      fontFaceRule({
        family: 'Montserrat',
        url: '/api/v2/branding/assets/font/abc.woff2',
        weight: '400',
        style: 'italic',
      }),
    ).toBe(
      '@font-face{font-family:"Montserrat";src:url("/api/v2/branding/assets/font/abc.woff2") format("woff2");font-display:swap;font-weight:400;font-style:italic;}',
    );
  });

  it('omits weight and style when absent, and drops a malformed weight', () => {
    expect(fontFaceRule({ family: 'Montserrat', url: '/f.woff2' })).toBe(
      '@font-face{font-family:"Montserrat";src:url("/f.woff2") format("woff2");font-display:swap;}',
    );
    expect(fontFaceRule({ family: 'Montserrat', url: '/f.woff2', weight: '400; color: red' })).not.toContain(
      'font-weight',
    );
    expect(fontFaceRule({ family: 'Montserrat', url: '/f.woff2', weight: '100 900' })).toContain(
      'font-weight:100 900',
    );
  });

  it('strips quote characters from the family so it cannot end the CSS string', () => {
    expect(fontFaceRule({ family: 'Mont"serrat', url: '/f.woff2' })).toContain('font-family:"Montserrat"');
  });

  it('refuses a non-same-origin url', () => {
    expect(fontFaceRule({ family: 'Montserrat', url: 'https://fonts.example.com/a.woff2' })).toBeUndefined();
  });
});

describe('fontFaceStylesheet', () => {
  it('declares the three brand faces of the default pack (brand guide v3), same-origin', () => {
    expect(DEFAULT_BRAND_PACK.typography.fontFaces?.map((face) => face.family)).toEqual(['Manrope', 'Sora', 'JetBrains Mono']);
    const css = fontFaceStylesheet(DEFAULT_BRAND_PACK);
    expect(css.split('\n')).toHaveLength(3);
    expect(css).toContain('src:url("./brand/fonts/manrope.woff2") format("woff2")');
  });

  it('is empty for a pack with no fontFaces', () => {
    const { fontFaces: _omitted, ...rest } = DEFAULT_BRAND_PACK.typography;
    expect(fontFaceStylesheet({ typography: rest })).toBe('');
  });

  it('joins accepted faces and logs each refused one', () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
    const css = fontFaceStylesheet({
      typography: {
        ...DEFAULT_BRAND_PACK.typography,
        fontFaces: [
          { family: 'Montserrat', url: '/a.woff2', weight: '400' },
          { family: 'Montserrat', url: 'https://evil.example/a.woff2', weight: '700' },
          { family: 'Montserrat', url: '/b.woff2', weight: '700' },
        ],
      },
    });
    expect(css.split('\n')).toHaveLength(2);
    expect(css).toContain('url("/a.woff2")');
    expect(css).toContain('url("/b.woff2")');
    expect(css).not.toContain('evil.example');
    expect(warn).toHaveBeenCalledTimes(1);
  });
});

describe('resolveFontUrl', () => {
  it('keeps ./ paths under a relative or root base (the main SPA; nginx serves brand/ at any depth)', () => {
    expect(resolveFontUrl('./brand/fonts/sora.woff2', './')).toBe('./brand/fonts/sora.woff2');
    expect(resolveFontUrl('./brand/fonts/sora.woff2', '/')).toBe('./brand/fonts/sora.woff2');
  });

  it('anchors ./ paths to an absolute base, so nested admin routes still find the face', () => {
    expect(resolveFontUrl('./brand/fonts/sora.woff2', '/admin/app/')).toBe('/admin/app/brand/fonts/sora.woff2');
    expect(fontFaceRule({ family: 'Sora', url: './brand/fonts/sora.woff2' }, '/admin/app/')).toContain('url("/admin/app/brand/fonts/sora.woff2")');
  });

  it('leaves root-relative and served asset paths alone', () => {
    expect(resolveFontUrl('/api/v2/branding/assets/font/abc.woff2', '/admin/app/')).toBe('/api/v2/branding/assets/font/abc.woff2');
  });
});
