import { render, waitFor } from '@testing-library/react-native';

import { FIXTURE_ORIGIN } from '@/api/__fixtures__/nativeClient';
import { BrandLogo } from '@/theme/BrandLogo';
import { base64Of, fetchBrandAsset, fetchBrandRaster } from '@/theme/brandAssets';
import { __resetBrandFontsForTests, loadBrandFaces } from '@/theme/brandFonts';

// SEC-3: raster logos and brand fonts were handed to <Image> and expo-font as
// URLs; both follow a redirect to any host, so a same-origin pack URL that
// redirected made the app fetch from (and parse font bytes of) a third party.
// They are now downloaded through the origin guard with redirects refused,
// and only the checked bytes reach the image view or the font loader.

const mockExpoFetch = jest.fn();
jest.mock('expo/fetch', () => ({ fetch: (...args: unknown[]) => mockExpoFetch(...args) }));
jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

const PNG = `${FIXTURE_ORIGIN}/api/v2/branding/assets/logo.png`;
const FONT = { alias: 'brand-Acme', url: `${FIXTURE_ORIGIN}/api/v2/branding/assets/acme.ttf` };
const BYTES = new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]);

beforeEach(() => {
  __resetBrandFontsForTests();
  mockExpoFetch.mockReset();
  jest.spyOn(console, 'info').mockImplementation(() => {});
});
afterEach(() => jest.restoreAllMocks());

const answer = (status: number, bytes: Uint8Array, type = 'image/png') =>
  new Response(bytes as unknown as BodyInit, { status, headers: { 'Content-Type': type } });

describe('BrandLogo raster', () => {
  it('renders the bytes fetched through the guard (data: URI), never the remote URL', async () => {
    const fetchRaster = jest.fn(async () => 'data:image/png;base64,iVBORw0KGgo=');
    const screen = await render(<BrandLogo image={{ uri: PNG, kind: 'raster' }} fetchRaster={fetchRaster} />);
    await waitFor(() => expect(screen.getByTestId('brand-raster').props.source).toEqual({ uri: 'data:image/png;base64,iVBORw0KGgo=' }));
    expect(fetchRaster).toHaveBeenCalledWith(PNG);
  });

  it('renders nothing when the download is refused', async () => {
    const fetchRaster = jest.fn(async () => null);
    const screen = await render(<BrandLogo image={{ uri: PNG, kind: 'raster' }} fetchRaster={fetchRaster} />);
    await waitFor(() => expect(fetchRaster).toHaveBeenCalled());
    expect(screen.queryByTestId('brand-raster')).toBeNull();
  });

  it('shows an inline data: raster from the pack as is', async () => {
    const fetchRaster = jest.fn();
    const screen = await render(<BrandLogo image={{ uri: 'data:image/png;base64,AAAA', kind: 'raster' }} fetchRaster={fetchRaster} />);
    expect(screen.getByTestId('brand-raster').props.source).toEqual({ uri: 'data:image/png;base64,AAAA' });
    expect(fetchRaster).not.toHaveBeenCalled();
  });
});

describe('brand fonts', () => {
  it('hands the font loader a local file of the guarded download, not the remote URL', async () => {
    const loader = jest.fn(async () => {});
    const download = jest.fn(async () => 'file:///cache/brand-Acme.ttf');
    expect(await loadBrandFaces([FONT], loader, download)).toEqual(['brand-Acme']);
    expect(download).toHaveBeenCalledWith(FONT);
    expect(loader).toHaveBeenCalledWith({ 'brand-Acme': { uri: 'file:///cache/brand-Acme.ttf' } });
  });

  it('uses no face when the download is refused', async () => {
    const loader = jest.fn(async () => {});
    expect(await loadBrandFaces([FONT], loader, async () => null)).toEqual([]);
    expect(loader).not.toHaveBeenCalled();
  });
});

describe('fetchBrandAsset', () => {
  it('downloads with redirects refused and returns the bytes', async () => {
    mockExpoFetch.mockResolvedValue(answer(200, BYTES));
    expect(await fetchBrandAsset(PNG, /^image\/png$/, 1000)).toEqual(BYTES);
    const [url, init] = mockExpoFetch.mock.calls[0] as [string, RequestInit];
    expect(url).toBe(PNG);
    expect(init.redirect).toBe('error');
  });

  it('refuses an unparseable URL, a failed or redirected request, a wrong type and an oversized body', async () => {
    expect(await fetchBrandAsset('not a url', /^image\/png$/, 1000)).toBeNull();
    expect(mockExpoFetch).not.toHaveBeenCalled();
    mockExpoFetch.mockRejectedValueOnce(new TypeError("Redirect is not allowed when redirect mode is 'error'"));
    expect(await fetchBrandAsset(PNG, /^image\/png$/, 1000)).toBeNull();
    mockExpoFetch.mockResolvedValueOnce(answer(404, BYTES));
    expect(await fetchBrandAsset(PNG, /^image\/png$/, 1000)).toBeNull();
    mockExpoFetch.mockResolvedValueOnce(answer(200, BYTES, 'text/html'));
    expect(await fetchBrandAsset(PNG, /^image\/png$/, 1000)).toBeNull();
    mockExpoFetch.mockResolvedValueOnce(answer(200, BYTES));
    expect(await fetchBrandAsset(PNG, /^image\/png$/, 4)).toBeNull();
  });

  it('turns a raster into a data: URI of the vetted type', async () => {
    mockExpoFetch.mockResolvedValue(answer(200, BYTES, 'image/png; charset=binary'));
    expect(await fetchBrandRaster(PNG)).toBe(`data:image/png;base64,${btoa(String.fromCharCode(...BYTES))}`);
  });

  it('encodes base64 like the platform does', () => {
    for (const n of [0, 1, 2, 3, 4, 5, 255]) {
      const bytes = Uint8Array.from({ length: n }, (_, i) => (i * 37 + 11) & 0xff);
      expect(base64Of(bytes)).toBe(btoa(String.fromCharCode(...bytes)));
    }
  });
});
