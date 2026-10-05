import { useMemo } from 'react';
import { useColorScheme } from 'react-native';

import { useBrandTheme } from '@/theme/brandStore';

import { Colors, type Palette } from './theme';

/**
 * The current palette: the fallback colours of the active scheme, with the
 * connected workspace's vetted brand overrides on top (src/theme/brandPack.ts).
 */
export function usePalette(): Palette {
  const scheme = useColorScheme() === 'dark' ? 'dark' : 'light';
  const brand = useBrandTheme();
  return useMemo(() => resolvePalette(scheme, brand?.[scheme]), [scheme, brand]);
}

export function resolvePalette(scheme: 'light' | 'dark', overrides: Partial<Palette> | undefined): Palette {
  return overrides ? { ...Colors[scheme], ...overrides } : Colors[scheme];
}
