import { useWindowDimensions } from 'react-native';

import { space } from '@/design/tokens';

import { uiPlatform, type UiPlatform } from './platform';

/**
 * The leading/trailing margin the platform's own bars use, so content placed right under a
 * header lines up with the large title and the search field (D7).
 *
 * iOS: UIKit's system layout margin, 16 pt on phones narrower than 414 pt and 20 pt on the
 * wide phones (Plus / Pro Max) and iPad; the navigation bar's large title and search field
 * sit on it. Android: the Material 3 top app bar's 16 dp.
 */
export function layoutMargin(windowWidth: number, on: UiPlatform = uiPlatform): number {
  if (on === 'android') return space.screenGutterCompact;
  return windowWidth >= 414 ? space.screenGutter : space.screenGutterCompact;
}

/** `layoutMargin` for the current window. */
export function useLayoutMargin(): number {
  const { width } = useWindowDimensions();
  return layoutMargin(width);
}
