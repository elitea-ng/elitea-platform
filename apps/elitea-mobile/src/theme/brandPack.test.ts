import { FIXTURE_ORIGIN, brandPack } from '@/api/__fixtures__/nativeClient';
import type { BaseFetch } from '@/api/middleware';
import {
  BrandPackSchema,
  acceptImage,
  fetchBrandTheme,
  firstFamily,
  mapBrandPack,
  mapScheme,
} from '@/theme/brandPack';
import { contrastRatio, parseColor } from '@/theme/color';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

const PACK_URL = `${FIXTURE_ORIGIN}/api/v2/branding/pack.json?v=4f1c2b9e0a7d`;

function packFetch(body: unknown, headers: Record<string, string> = {}, status = 200) {
  const seen: string[] = [];
  const fetch: jest.MockedFunction<BaseFetch> = jest.fn(async (input: Request) => {
    seen.push(input.url);
    return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json', ...headers } });
  });
  return { fetch, seen };
}

beforeEach(() => {
  jest.spyOn(console, 'warn').mockImplementation(() => {});
  jest.spyOn(console, 'info').mockImplementation(() => {});
});
afterEach(() => jest.restoreAllMocks());

describe('mapScheme', () => {
  it('maps the allowlisted tokens of the product default pack', () => {
    const pack = brandPack();
    expect(mapScheme(pack.schemes.light, pack.brand.hue)).toEqual({
      text: '#0e131d',
      textSecondary: '#777a83',
      background: '#f8fcff',
      backgroundElement: '#ffffff',
      border: '#cbced6',
      accent: '#c428dd',
      // The pack's near-white label reaches only ~4.2:1 on this accent, below
      // WCAG AA for normal text, so the higher-contrast of black/white is used.
      onAccent: '#000000',
      danger: '#d71616',
    });
    expect(mapScheme(pack.schemes.dark, pack.brand.hue)).toMatchObject({
      text: '#ffffff',
      background: '#0e131d',
      accent: '#6ae8fa',
      onAccent: '#0e131d',
    });
  });

  it('ignores gradients, url(), expressions and garbage', () => {
    const tokens = {
      'background.default': '#ffffff',
      'text.secondary': '#000000',
      'primary.main': 'linear-gradient(1deg, #000 0%, #fff 100%)',
      'error.main': 'url(https://evil.example/x)',
      'border.lines': 'expression(alert(1))',
      'text.primary': 'javascript:alert(1)',
    };
    expect(mapScheme(tokens, 'not-a-colour')).toEqual({ background: '#ffffff', text: '#000000' });
  });

  it('falls back to the brand hue for the accent', () => {
    expect(mapScheme({ 'background.default': '#ffffff', 'text.secondary': '#000000' }, '#7b2ff7').accent).toBe('#7b2ff7');
  });

  it('drops the whole scheme when text is unreadable on the background', () => {
    expect(mapScheme({ 'background.default': '#ffffff', 'text.secondary': '#eeeeee', 'primary.main': '#c428dd' }, '')).toEqual({});
  });

  it('drops the whole scheme when the background is translucent', () => {
    expect(mapScheme({ 'background.default': 'rgba(255, 255, 255, 0.5)', 'text.secondary': '#000000' }, '')).toEqual({});
  });

  it('drops a single weak token and repairs unreadable button text', () => {
    const out = mapScheme(
      {
        'background.default': '#ffffff',
        'text.secondary': '#000000',
        'text.primary': '#f0f0f0', // muted text below 3:1: dropped
        'primary.main': '#0b57d0',
        'text.button.primary': '#0c58d1', // invisible on the accent: replaced
      },
      '',
    );
    expect(out.textSecondary).toBeUndefined();
    expect(out.onAccent).toBe('#ffffff');
    expect(contrastRatio(parseColor(out.onAccent)!, parseColor(out.accent)!)).toBeGreaterThanOrEqual(4.5);
  });
});

describe('acceptImage', () => {
  it.each([
    [`${FIXTURE_ORIGIN}/brand/logo-mark.svg`, 'svg'],
    [`${FIXTURE_ORIGIN}/api/v2/branding/assets/abc.png?v=1`, 'raster'],
    ['data:image/png;base64,iVBORw0KGgo=', 'raster'],
  ])('accepts %s', (uri, kind) => {
    expect(acceptImage(uri, FIXTURE_ORIGIN)).toEqual({ uri, kind });
  });

  it.each([
    'https://cdn.example.net/logo.svg',
    'http://ai.example.com/logo.svg',
    '//ai.example.com/logo.svg',
    '/brand/logo.svg',
    `${FIXTURE_ORIGIN}/brand/../../etc/logo.svg`,
    `${FIXTURE_ORIGIN}/brand/logo.gif`,
    'data:image/svg+xml;base64,PHN2Zz4=',
    'javascript:alert(1)',
  ])('refuses %s', (uri) => {
    expect(acceptImage(uri, FIXTURE_ORIGIN)).toBeNull();
  });
});

describe('fonts', () => {
  it('reads the first family of a stack', () => {
    expect(firstFamily('"Montserrat", Roboto, sans-serif')).toBe('Montserrat');
    expect(firstFamily("url(x), Arial")).toBeNull();
    expect(firstFamily(undefined)).toBeNull();
  });

  it('uses system fonts when the pack ships no face for its family', () => {
    const theme = mapBrandPack(BrandPackSchema.parse(brandPack()), FIXTURE_ORIGIN, 'default');
    expect(theme).toMatchObject({ fontFaces: [], fontFamily: null });
  });

  it('loads only a same-origin TrueType/OpenType face of the first family', () => {
    const pack = brandPack({
      typography: {
        fontFamily: '"Acme Sans", sans-serif',
        fontFaces: [
          { family: 'Acme Sans', url: 'https://fonts.example.net/acme.ttf' },
          { family: 'Acme Sans', url: `${FIXTURE_ORIGIN}/api/v2/branding/assets/acme.woff2` },
          { family: 'Acme Sans', url: `${FIXTURE_ORIGIN}/api/v2/branding/assets/acme.ttf` },
        ],
      },
    });
    const theme = mapBrandPack(BrandPackSchema.parse(pack), FIXTURE_ORIGIN, 'file');
    expect(theme.fontFaces).toEqual([{ alias: 'brand-Acme-Sans', url: `${FIXTURE_ORIGIN}/api/v2/branding/assets/acme.ttf` }]);
    expect(theme.fontFamily).toBe('brand-Acme-Sans');
  });
});

describe('fetchBrandTheme', () => {
  it('fetches the discovery pack URL anonymously and maps it', async () => {
    const { fetch, seen } = packFetch(brandPack(), { 'X-Elitea-Brand-Layers': 'default' });
    const theme = await fetchBrandTheme(FIXTURE_ORIGIN, PACK_URL, { fetch });
    expect(seen).toEqual([PACK_URL]);
    expect(theme).toMatchObject({
      origin: FIXTURE_ORIGIN,
      productName: 'Elitea',
      productDefault: true,
      logoMark: { uri: `${FIXTURE_ORIGIN}/brand/logo-mark.svg`, kind: 'svg' },
    });
  });

  it('marks a deployment brand as such', async () => {
    const { fetch } = packFetch(brandPack(), { 'X-Elitea-Brand-Layers': 'file, db' });
    expect((await fetchBrandTheme(FIXTURE_ORIGIN, PACK_URL, { fetch }))?.productDefault).toBe(false);
  });

  it.each([
    'https://evil.example/api/v2/branding/pack.json',
    `${FIXTURE_ORIGIN}/api/v2/other.json`,
  ])('refuses a pack URL that is not the workspace pack route: %s', async (url) => {
    const { fetch } = packFetch(brandPack());
    expect(await fetchBrandTheme(FIXTURE_ORIGIN, url, { fetch })).toBeNull();
    expect(fetch).not.toHaveBeenCalled();
  });

  it('returns null for a bad body or status', async () => {
    expect(await fetchBrandTheme(FIXTURE_ORIGIN, PACK_URL, packFetch({ nope: 1 }))).toBeNull();
    expect(await fetchBrandTheme(FIXTURE_ORIGIN, PACK_URL, packFetch(brandPack(), {}, 503))).toBeNull();
  });
});
