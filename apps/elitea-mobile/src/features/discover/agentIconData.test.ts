import { FIXTURE_ORIGIN } from '@/api/__fixtures__/nativeClient';
import { fetchPublicAgentIcon, rasterTypeOf } from '@/features/discover/agentIconData';
import { MAX_RASTER_BYTES } from '@/theme/brandAssets';

// SEC-3 for Discover: a publisher-chosen icon path on the workspace origin
// that redirected sent every viewer's device to the publisher's host through
// <Image>. The icon is now downloaded with redirects refused, its type and
// size checked, and its bytes must carry an image signature.

const mockExpoFetch = jest.fn();
jest.mock('expo/fetch', () => ({ fetch: (...args: unknown[]) => mockExpoFetch(...args) }));
jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

const ICON = `${FIXTURE_ORIGIN}/icons/1/076958cfcd17545b.png`;
const PNG = new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 0x0d]);
const JPEG = new Uint8Array([0xff, 0xd8, 0xff, 0xe0, 0, 0x10]);
const WEBP = new Uint8Array([0x52, 0x49, 0x46, 0x46, 4, 0, 0, 0, 0x57, 0x45, 0x42, 0x50]);
const HTML = new TextEncoder().encode('<html><body>sign in</body></html>');

const answer = (status: number, bytes: Uint8Array, type = 'image/png', headers: Record<string, string> = {}) =>
  new Response(bytes as unknown as BodyInit, { status, headers: { 'Content-Type': type, ...headers } });
const dataOf = (mime: string, bytes: Uint8Array) => `data:${mime};base64,${btoa(String.fromCharCode(...bytes))}`;

beforeEach(() => {
  mockExpoFetch.mockReset();
  jest.spyOn(console, 'info').mockImplementation(() => {});
});
afterEach(() => jest.restoreAllMocks());

describe('fetchPublicAgentIcon', () => {
  it('downloads with redirects refused and no credential, and answers a data: URI of the bytes', async () => {
    mockExpoFetch.mockResolvedValue(answer(200, PNG));
    expect(await fetchPublicAgentIcon(ICON)).toBe(dataOf('image/png', PNG));
    const [url, init] = mockExpoFetch.mock.calls[0] as [string, RequestInit];
    expect(url).toBe(ICON);
    expect(init.redirect).toBe('error');
    expect(new Headers(init.headers).get('Authorization')).toBeNull();
  });

  it('takes the type from the bytes: a JPEG or WebP is shown as what it is', async () => {
    mockExpoFetch.mockResolvedValueOnce(answer(200, JPEG, 'image/png'));
    expect(await fetchPublicAgentIcon(ICON)).toBe(dataOf('image/jpeg', JPEG));
    mockExpoFetch.mockResolvedValueOnce(answer(200, WEBP, 'image/webp'));
    expect(await fetchPublicAgentIcon(ICON)).toBe(dataOf('image/webp', WEBP));
  });

  it('a redirect (3xx), a non-200, a non-image type, an oversized body or bytes that are not an image give null', async () => {
    mockExpoFetch.mockRejectedValueOnce(new TypeError("Redirect is not allowed when redirect mode is 'error'"));
    expect(await fetchPublicAgentIcon(ICON)).toBeNull();
    mockExpoFetch.mockResolvedValueOnce(answer(302, new Uint8Array(), 'text/html', { Location: 'https://tracker.example.net/p.png' }));
    expect(await fetchPublicAgentIcon(ICON)).toBeNull();
    mockExpoFetch.mockResolvedValueOnce(answer(404, PNG));
    expect(await fetchPublicAgentIcon(ICON)).toBeNull();
    mockExpoFetch.mockResolvedValueOnce(answer(200, PNG, 'image/svg+xml'));
    expect(await fetchPublicAgentIcon(ICON)).toBeNull();
    mockExpoFetch.mockResolvedValueOnce(answer(200, HTML, 'image/png'));
    expect(await fetchPublicAgentIcon(ICON)).toBeNull();
    mockExpoFetch.mockResolvedValueOnce(answer(200, PNG, 'image/png', { 'Content-Length': String(MAX_RASTER_BYTES + 1) }));
    expect(await fetchPublicAgentIcon(ICON)).toBeNull();
  });

  it('never leaves the workspace origin it was given', async () => {
    expect(await fetchPublicAgentIcon('not a url')).toBeNull();
    expect(mockExpoFetch).not.toHaveBeenCalled();
  });
});

it('recognises PNG, JPEG and WebP signatures only', () => {
  expect(rasterTypeOf(PNG)).toBe('image/png');
  expect(rasterTypeOf(JPEG)).toBe('image/jpeg');
  expect(rasterTypeOf(WEBP)).toBe('image/webp');
  expect(rasterTypeOf(HTML)).toBeNull();
  expect(rasterTypeOf(new Uint8Array())).toBeNull();
});
