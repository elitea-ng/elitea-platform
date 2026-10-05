import { FIXTURE_ORIGIN, brandPack } from '@/api/__fixtures__/nativeClient';
import { resolvePalette } from '@/constants/use-palette';
import { Colors } from '@/constants/theme';
import { endSession } from '@/security/endSession';
import { SECURE_KEYS, getSecureItem, setSecureItem } from '@/security/secureStorage';
import { __resetSessionForTests, setSession } from '@/state/session';
import { BrandPackSchema, mapBrandPack, type BrandTheme } from '@/theme/brandPack';
import {
  __resetBrandThemeForTests,
  applyBrandFromDiscovery,
  getBrandTheme,
  restoreBrandTheme,
  setBrandTheme,
} from '@/theme/brandStore';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

const PACK_URL = `${FIXTURE_ORIGIN}/api/v2/branding/pack.json?v=1`;
const theme: BrandTheme = mapBrandPack(BrandPackSchema.parse(brandPack()), FIXTURE_ORIGIN, 'file');

beforeEach(() => {
  __resetSessionForTests();
  __resetBrandThemeForTests();
  setSession({ hydrated: true, serverOrigin: FIXTURE_ORIGIN });
});

const flush = () => new Promise((resolve) => setTimeout(resolve, 0));

it('applies the pack named by discovery once per pack URL', async () => {
  const fetchTheme = jest.fn(async () => theme);
  await applyBrandFromDiscovery(FIXTURE_ORIGIN, { brand_pack_url: PACK_URL }, fetchTheme);
  await applyBrandFromDiscovery(FIXTURE_ORIGIN, { brand_pack_url: PACK_URL }, fetchTheme);
  expect(fetchTheme).toHaveBeenCalledTimes(1);
  expect(getBrandTheme()).toBe(theme);
  await applyBrandFromDiscovery(FIXTURE_ORIGIN, { brand_pack_url: `${PACK_URL}2` }, fetchTheme);
  expect(fetchTheme).toHaveBeenCalledTimes(2);
});

it('drops a pack that arrives after the workspace changed', async () => {
  const fetchTheme = jest.fn(async () => {
    setSession({ serverOrigin: 'https://other.example.com' });
    return theme;
  });
  await applyBrandFromDiscovery(FIXTURE_ORIGIN, { brand_pack_url: PACK_URL }, fetchTheme);
  expect(getBrandTheme()).toBeNull();
});

it('keeps the fallback theme when the pack is unavailable', async () => {
  await applyBrandFromDiscovery(FIXTURE_ORIGIN, { brand_pack_url: PACK_URL }, async () => null);
  expect(getBrandTheme()).toBeNull();
  expect(resolvePalette('light', getBrandTheme()?.light)).toBe(Colors.light);
});

it('persists the mapped theme and restores it at launch for the same workspace', async () => {
  setBrandTheme(theme, PACK_URL);
  await flush();
  __resetBrandThemeForTests();
  await restoreBrandTheme(FIXTURE_ORIGIN);
  expect(getBrandTheme()).toMatchObject({ productName: 'Elitea', light: theme.light, dark: theme.dark });
  expect(resolvePalette('dark', getBrandTheme()?.dark).accent).toBe('#6ae8fa');
});

it('re-validates a restored theme (a tampered value cannot widen what is loaded)', async () => {
  const tampered = {
    ...theme,
    logoMark: { uri: 'https://evil.example/logo.svg', kind: 'svg' },
    light: { ...theme.light, accent: 'url(https://evil.example/x)', text: '#fefefe' },
    fontFaces: [{ alias: 'brand-x', url: 'https://evil.example/x.ttf' }],
  };
  await setSecureItem(SECURE_KEYS.brandTheme, JSON.stringify(tampered));
  await restoreBrandTheme(FIXTURE_ORIGIN);
  const restored = getBrandTheme()!;
  expect(restored.logoMark).toBeNull();
  expect(restored.light).toEqual({}); // unreadable text: scheme dropped
  expect(restored.fontFaces).toEqual([]);
});

it('ignores a persisted theme of another workspace', async () => {
  setBrandTheme(theme, PACK_URL);
  await flush();
  __resetBrandThemeForTests();
  await restoreBrandTheme('https://other.example.com');
  expect(getBrandTheme()).toBeNull();
});

it('is wiped with the workspace', async () => {
  setBrandTheme(theme, PACK_URL);
  await flush();
  await endSession({ reason: 'server_switch' });
  expect(getBrandTheme()).toBeNull();
  expect(await getSecureItem(SECURE_KEYS.brandTheme)).toBeNull();
});

// F7: a pack fetched while the session is ending must neither show nor be persisted.
it('drops a pack that arrives while the session is ending', async () => {
  let answer!: (value: BrandTheme) => void;
  const pending = applyBrandFromDiscovery(FIXTURE_ORIGIN, { brand_pack_url: PACK_URL }, () => new Promise((resolve) => (answer = resolve)));
  setSession({ ending: true });
  answer(theme);
  await pending;
  await flush();
  expect(getBrandTheme()).toBeNull();
  expect(await getSecureItem(SECURE_KEYS.brandTheme)).toBeNull();
});
