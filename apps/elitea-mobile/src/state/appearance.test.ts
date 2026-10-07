import { Appearance } from 'react-native';

import { paletteFromDesign } from '@/constants/use-palette';
import { colors } from '@/design/tokens';
import { DEVICE_KEYS } from '@/security/secureStorage';
import {
  getSchemePreference,
  hydrateSchemePreference,
  parseSchemePreference,
  resetSchemePreferenceForTests,
  setSchemePreference,
} from '@/state/appearance';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

describe('Settings → Appearance', () => {
  let setColorScheme: jest.SpyInstance;
  beforeEach(() => {
    resetSchemePreferenceForTests();
    setColorScheme = jest.spyOn(Appearance, 'setColorScheme').mockImplementation(() => {});
  });
  afterEach(() => setColorScheme.mockRestore());

  it('accepts only system, light and dark', () => {
    expect(parseSchemePreference('dark')).toBe('dark');
    expect(parseSchemePreference('sepia')).toBe('system');
    expect(parseSchemePreference(null)).toBe('system');
  });

  it('applies the choice to the native chrome at once, stores it, and restores it on launch', async () => {
    await setSchemePreference('dark');
    expect(setColorScheme).toHaveBeenLastCalledWith('dark');
    expect(getSchemePreference()).toBe('dark');
    resetSchemePreferenceForTests();
    await hydrateSchemePreference();
    expect(getSchemePreference()).toBe('dark');
    await setSchemePreference('system');
    expect(setColorScheme).toHaveBeenLastCalledWith('auto');
  });

  it('is a device item (not wiped with one workspace)', () => {
    expect(DEVICE_KEYS.appearance).toBe('az.appearance');
  });
});

describe('the legacy palette is a view of the design tokens', () => {
  it.each(['light', 'dark'] as const)('%s: Zefir neutrals and the accent family, never the pack neutrals', (scheme) => {
    const c = colors[scheme];
    expect(paletteFromDesign(c)).toEqual({
      text: c.textPrimary,
      textSecondary: c.textSecondary,
      background: c.background,
      backgroundElement: c.surface,
      border: c.separator,
      accent: c.accent,
      onAccent: c.accentOn,
      danger: c.danger,
      inputBorder: c.separatorStrong,
      field: c.surfaceSunken,
    });
  });
});
