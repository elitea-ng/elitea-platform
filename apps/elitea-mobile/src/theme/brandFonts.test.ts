import { __resetBrandFontsForTests, loadBrandFaces } from '@/theme/brandFonts';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

beforeEach(() => {
  __resetBrandFontsForTests();
  jest.spyOn(console, 'info').mockImplementation(() => {});
});
afterEach(() => jest.restoreAllMocks());

const face = { alias: 'brand-Acme', url: 'https://ai.example.com/api/v2/branding/assets/acme.ttf' };

it('loads a vetted face once, under its namespaced alias', async () => {
  const loader = jest.fn(async () => {});
  const download = jest.fn(async () => 'file:///cache/brand-Acme.ttf');
  expect(await loadBrandFaces([face], loader, download)).toEqual(['brand-Acme']);
  expect(await loadBrandFaces([face], loader, download)).toEqual(['brand-Acme']);
  expect(loader).toHaveBeenCalledTimes(1);
  expect(loader).toHaveBeenCalledWith({ 'brand-Acme': { uri: 'file:///cache/brand-Acme.ttf' } });
});

it('falls back to the system font when a face fails to load', async () => {
  const loader = jest.fn(async () => {
    throw new Error('download failed');
  });
  expect(await loadBrandFaces([face], loader, async () => 'file:///cache/brand-Acme.ttf')).toEqual([]);
});
