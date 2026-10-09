import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { BRAND_PACK_GLOBAL, type BrandPack, DEFAULT_BRAND_PACK, hasServedBrandPack, resolveBrandAsset, resolveBrandPack } from '@/shared/brand';
import { clearNamespace } from '@/shared/lib/storage';

import {
  BRAND_CACHE_KEY,
  DESKTOP_FONT_STYLE_ATTRIBUTE,
  applyDesktopBrand,
  desktopFontStylesheet,
  loadDesktopBrand,
  parseBrandPayload,
  readCachedBrand,
  refreshBrand,
  writeCachedBrand,
} from './brandPack';

import { installWebStorageShim } from '../../test/webstorage';

installWebStorageShim();

const ORIGIN = 'https://elitea.example.com';
const SVG = '<svg xmlns="http://www.w3.org/2000/svg"/>';
const WOFF2 = new Uint8Array([0x77, 0x4f, 0x46, 0x32, 1, 2, 3]);

/** A branded pack as pack.json serves it: every reference absolute. */
function servedPack(overrides: Partial<BrandPack['assets']> = {}): BrandPack {
  return {
    ...DEFAULT_BRAND_PACK,
    id: 'acme',
    product: { ...DEFAULT_BRAND_PACK.product, name: 'Acme AI' },
    assets: {
      logoFull: `${ORIGIN}/api/v2/branding/assets/logo-full/aa.svg`,
      // The product default's own artwork, absolutised by the server.
      logoMark: `${ORIGIN}/app/brand/logo-mark.svg`,
      favicon: `${ORIGIN}/api/v2/branding/assets/favicon/bb.png`,
      ...overrides,
    },
    typography: {
      ...DEFAULT_BRAND_PACK.typography,
      fontFamily: '"Acme Sans", Arial, sans-serif',
      fontFaces: [{ family: 'Acme Sans', url: `${ORIGIN}/api/v2/branding/assets/font/cc.woff2`, weight: '400' }],
    },
  };
}

interface Route {
  status?: number;
  body?: BodyInit;
  headers?: Record<string, string>;
}

function fakeFetch(routes: Record<string, Route | (() => Route)>) {
  return vi.fn((input: string, _init?: RequestInit): Promise<Response> => {
    const entry = routes[input];
    if (entry === undefined) return Promise.resolve(new Response('', { status: 404 }));
    const route = typeof entry === 'function' ? entry() : entry;
    const status = route.status ?? 200;
    return Promise.resolve(new Response(status === 304 ? null : (route.body ?? ''), { status, headers: route.headers ?? {} }));
  });
}

function brandedRoutes(pack: BrandPack = servedPack(), layers = 'file, db'): Record<string, Route> {
  return {
    [`${ORIGIN}/api/v2/branding/pack.json`]: {
      body: JSON.stringify(pack),
      headers: { 'content-type': 'application/json', etag: '"v1"', 'x-elitea-brand-layers': layers },
    },
    [`${ORIGIN}/api/v2/branding/assets/logo-full/aa.svg`]: { body: SVG, headers: { 'content-type': 'image/svg+xml' } },
    [`${ORIGIN}/api/v2/branding/assets/favicon/bb.png`]: { body: new Uint8Array([137, 80, 78, 71]), headers: { 'content-type': 'image/png' } },
    [`${ORIGIN}/api/v2/branding/assets/font/cc.woff2`]: { body: WOFF2, headers: { 'content-type': 'font/woff2' } },
  };
}

function globals(): Record<string, unknown> {
  return globalThis;
}

beforeEach(() => {
  clearNamespace();
  delete globals()[BRAND_PACK_GLOBAL];
  document.head.querySelector(`style[${DESKTOP_FONT_STYLE_ATTRIBUTE}]`)?.remove();
});

afterEach(() => {
  vi.restoreAllMocks();
  delete globals()[BRAND_PACK_GLOBAL];
});

describe('parseBrandPayload', () => {
  it('reads pack.json as JSON', () => {
    expect(parseBrandPayload('{"id":"acme"}')).toEqual({ id: 'acme' });
  });

  it('reads the bootstrap.js assignment as JSON without evaluating it', () => {
    expect(parseBrandPayload('window.elitea_brand = {"id":"acme","product":{"name":"A"}};')).toEqual({
      id: 'acme',
      product: { name: 'A' },
    });
  });

  it('reads the inert "no pack configured" body as unbranded', () => {
    expect(parseBrandPayload('/* elitea: no deployment brand pack configured */\n')).toBeNull();
  });

  it.each([
    ['a second statement', 'window.elitea_brand = {"id":"x"}; alert(1);'],
    ['a call', 'window.elitea_brand = fetch("https://evil")'],
    ['a function', 'window.elitea_brand = (() => ({ id: "x" }))();'],
    ['an object literal that is not JSON', 'window.elitea_brand = { id: "x" };'],
    ['a different global', 'window.other = {"id":"x"};'],
    ['code after a comment', '/* x */ alert(1) /* y */'],
    ['an array', '[1,2]'],
    ['JSON null', 'null'],
    ['garbage', '<html>'],
  ])('refuses %s', (_label, payload) => {
    expect(parseBrandPayload(payload)).toBeUndefined();
  });
});

describe('refreshBrand', () => {
  it('validates, inlines same-origin assets as data: URIs and moves faces out of the pack', async () => {
    const fetchMock = fakeFetch(brandedRoutes());
    const entry = await refreshBrand(fetchMock, ORIGIN);
    if (entry?.pack == null) throw new Error('expected a prepared pack');
    const { pack, fonts } = entry;

    expect(entry.etag).toBe('"v1"');
    expect(pack.product.name).toBe('Acme AI');
    expect(pack.assets.logoFull).toBe(`data:image/svg+xml;base64,${btoa(SVG)}`);
    expect(pack.assets.favicon).toMatch(/^data:image\/png;base64,/);
    // The default's own artwork stays the compiled default (not "custom").
    expect(pack.assets.logoMark).toBe(DEFAULT_BRAND_PACK.assets.logoMark);
    expect(pack.typography.fontFaces).toBeUndefined();
    expect(fonts).toHaveLength(1);
    expect(fonts[0]).toMatchObject({ family: 'Acme Sans', weight: '400' });
    expect(fonts[0]?.src).toMatch(/^data:font\/woff2;base64,/);
    // Public endpoint: no credentials, no redirects.
    expect(fetchMock.mock.calls[0]?.[1]).toMatchObject({ credentials: 'omit', redirect: 'error' });
  });

  it('never fetches a foreign asset and falls back to the default artwork', async () => {
    const fetchMock = fakeFetch(brandedRoutes(servedPack({ favicon: 'https://evil.example/f.png' })));
    const entry = await refreshBrand(fetchMock, ORIGIN);
    expect(entry?.pack?.assets.favicon).toBe(DEFAULT_BRAND_PACK.assets.favicon);
    expect(fetchMock.mock.calls.map(([url]) => url)).not.toContain('https://evil.example/f.png');
  });

  it('refuses an asset whose content type the slot does not take', async () => {
    const routes = brandedRoutes();
    routes[`${ORIGIN}/api/v2/branding/assets/logo-full/aa.svg`] = { body: '<script/>', headers: { 'content-type': 'text/html' } };
    const entry = await refreshBrand(fakeFetch(routes), ORIGIN);
    expect(entry?.pack?.assets.logoFull).toBe(DEFAULT_BRAND_PACK.assets.logoFull);
  });

  it('treats a product-default-only answer as unbranded, as the web does', async () => {
    const entry = await refreshBrand(fakeFetch(brandedRoutes(servedPack(), 'default')), ORIGIN);
    expect(entry).toMatchObject({ pack: null, fonts: [] });
  });

  it('degrades a schema-invalid pack to the compiled default', async () => {
    vi.spyOn(console, 'warn').mockImplementation(() => undefined);
    const routes = brandedRoutes();
    routes[`${ORIGIN}/api/v2/branding/pack.json`] = { body: '{"id":"acme"}', headers: { 'x-elitea-brand-layers': 'db' } };
    const entry = await refreshBrand(fakeFetch(routes), ORIGIN);
    expect(entry).toMatchObject({ pack: null });
  });

  it('falls back to bootstrap.js, parsed as data, on a deployment without pack.json', async () => {
    const legacy = servedPack({ logoFull: '/api/v2/branding/assets/logo-full/aa.svg', logoMark: './brand/logo-mark.svg' });
    const routes: Record<string, Route> = {
      ...brandedRoutes(),
      [`${ORIGIN}/api/v2/branding/pack.json`]: { status: 404 },
      [`${ORIGIN}/api/v2/branding/bootstrap.js`]: { body: `window.elitea_brand = ${JSON.stringify(legacy)};` },
    };
    const entry = await refreshBrand(fakeFetch(routes), ORIGIN);
    expect(entry?.pack?.assets.logoFull).toMatch(/^data:image\/svg\+xml;base64,/);
    expect(entry?.pack?.assets.logoMark).toBe('./brand/logo-mark.svg');
    expect(entry?.etag).toBeNull();
  });

  it('answers the cached entry on 304', async () => {
    const cached = { origin: ORIGIN, etag: '"v1"', pack: null, fonts: [] };
    const fetchMock = fakeFetch({ [`${ORIGIN}/api/v2/branding/pack.json`]: { status: 304 } });
    expect(await refreshBrand(fetchMock, ORIGIN, cached)).toBe(cached);
    expect(fetchMock.mock.calls[0]?.[1]?.headers).toMatchObject({ 'If-None-Match': '"v1"' });
  });

  it('resolves undefined when the deployment cannot be reached', async () => {
    expect(await refreshBrand(vi.fn().mockRejectedValue(new TypeError('offline')), ORIGIN)).toBeUndefined();
  });
});

describe('cache', () => {
  it('stores the prepared pack per deployment under the el. namespace, which the logout sweep clears', async () => {
    await refreshBrand(fakeFetch(brandedRoutes()), ORIGIN);
    expect(window.localStorage.getItem(`el.${BRAND_CACHE_KEY}`)).not.toBeNull();
    expect(readCachedBrand(`${ORIGIN}/`)?.pack?.product.name).toBe('Acme AI');
    expect(readCachedBrand('https://other.example.com')).toBeUndefined();

    clearNamespace();
    expect(readCachedBrand(ORIGIN)).toBeUndefined();
  });

  it('treats a corrupt entry as absent', () => {
    window.localStorage.setItem(`el.${BRAND_CACHE_KEY}`, '{"origin":"https://elitea.example.com","fonts":[],"pack":{"id":1}}');
    expect(readCachedBrand(ORIGIN)).toBeUndefined();
    window.localStorage.setItem(`el.${BRAND_CACHE_KEY}`, 'not json');
    expect(readCachedBrand(ORIGIN)).toBeUndefined();
  });

  it('drops a cached face whose source is not an inlined woff2', () => {
    writeCachedBrand({ origin: ORIGIN, etag: null, pack: null, fonts: [{ family: 'X', src: 'https://evil.example/x.woff2' }] });
    expect(readCachedBrand(ORIGIN)).toBeUndefined();
  });
});

describe('applyDesktopBrand', () => {
  it('publishes the pack where channel C reads it, so the shared readers see the deployment brand', async () => {
    const entry = await refreshBrand(fakeFetch(brandedRoutes()), ORIGIN);
    applyDesktopBrand(entry!);

    expect(hasServedBrandPack()).toBe(true);
    expect(resolveBrandPack().product.name).toBe('Acme AI');
    expect(resolveBrandAsset('logoFull').custom).toBe(true);
    expect(resolveBrandAsset('logoFull').url).toMatch(/^data:image\/svg\+xml/);
    expect(resolveBrandAsset('logoMark').custom).toBe(false);
    expect(resolveBrandAsset('favicon').custom).toBe(true);
    const style = document.head.querySelector(`style[${DESKTOP_FONT_STYLE_ATTRIBUTE}]`);
    expect(style?.textContent).toContain('font-family:"Acme Sans"');
    expect(style?.textContent).toContain('src:url("data:font/woff2;base64,');
  });

  it('leaves the global unset for an unbranded deployment', () => {
    globals()[BRAND_PACK_GLOBAL] = { stale: true };
    applyDesktopBrand({ pack: null, fonts: [] });
    expect(hasServedBrandPack()).toBe(false);
    expect(resolveBrandPack()).toBe(DEFAULT_BRAND_PACK);
    expect(document.head.querySelector(`style[${DESKTOP_FONT_STYLE_ATTRIBUTE}]`)).toBeNull();
  });

  it('never emits a face whose source could break out of the CSS url()', () => {
    expect(desktopFontStylesheet([{ family: 'X', src: 'data:font/woff2;base64,AA");} body{background:red' }])).toBe('');
    expect(desktopFontStylesheet([{ family: 'X"}', src: 'data:font/woff2;base64,AAAA', weight: '400;color:red' }])).toBe(
      '@font-face{font-family:"X}";src:url("data:font/woff2;base64,AAAA") format("woff2");font-display:swap;}',
    );
  });
});

describe('loadDesktopBrand', () => {
  it('waits for the first pack when nothing is cached, then paints from the cache next launch', async () => {
    const fetchMock = fakeFetch(brandedRoutes());
    await loadDesktopBrand({ fetch: fetchMock, origin: ORIGIN });
    expect(resolveBrandPack().product.name).toBe('Acme AI');

    delete globals()[BRAND_PACK_GLOBAL];
    let release: (route: Route) => void = () => undefined;
    const slow = new Promise<Route>((resolve) => {
      release = resolve;
    });
    const nextFetch = vi.fn(async (input: string) => {
      if (input.endsWith('/pack.json')) {
        const route = await slow;
        return new Response(null, { status: route.status ?? 200 });
      }
      return new Response('', { status: 404 });
    });
    const { refreshed } = await loadDesktopBrand({ fetch: nextFetch, origin: ORIGIN });
    // Applied from the cache before the background refresh answered.
    expect(resolveBrandPack().product.name).toBe('Acme AI');
    release({ status: 304 });
    expect((await refreshed)?.pack?.product.name).toBe('Acme AI');
  });

  it('mounts with the default when the first answer is too slow, and still caches it', async () => {
    vi.useFakeTimers();
    try {
      let release: () => void = () => undefined;
      const gate = new Promise<void>((resolve) => {
        release = resolve;
      });
      const routes = fakeFetch(brandedRoutes());
      const slowFetch = vi.fn(async (input: string, init?: RequestInit) => {
        await gate;
        return routes(input, init);
      });
      const loading = loadDesktopBrand({ fetch: slowFetch, origin: ORIGIN, firstLoadTimeoutMs: 100 });
      await vi.advanceTimersByTimeAsync(100);
      const { refreshed } = await loading;
      expect(hasServedBrandPack()).toBe(false);
      release();
      await refreshed;
      expect(readCachedBrand(ORIGIN)?.pack?.product.name).toBe('Acme AI');
    } finally {
      vi.useRealTimers();
    }
  });
});
