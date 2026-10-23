import type { SemanticColors } from '@/design/tokens';

import type { UiPlatform } from './platform';
import { uiPlatform } from './platform';

/**
 * Props of the app's `NativeTabs` (docs/design-system.md §4.12), shared by the signed-in tabs
 * and the design gallery's tab preview so both draw the same bar.
 *
 * iOS 26+: the system Liquid Glass tab bar, minimised while scrolling down, SF Symbols with the
 * filled variant when selected, tinted with the (corrected) accent; a sidebar on iPad. The
 * system draws the bar itself, so no colours beyond the tint are set on iOS.
 * Android: the Material 3 navigation bar, labels always shown, the active indicator pill in
 * `accentSoft` with the icon in `accentSoftOn`, a `surface` bar under edge-to-edge.
 */
export function nativeTabsProps(colors: SemanticColors, on: UiPlatform = uiPlatform) {
  const shared = {
    tintColor: colors.accent,
    minimizeBehavior: 'onScrollDown' as const,
    sidebarAdaptable: true,
    badgeBackgroundColor: colors.badge,
    badgeTextColor: colors.badgeText,
  };
  if (on !== 'android') return shared;
  return {
    ...shared,
    backgroundColor: colors.surface,
    indicatorColor: colors.accentSoft,
    rippleColor: colors.accentSoft,
    iconColor: { default: colors.textSecondary, selected: colors.accentSoftOn },
    labelStyle: { default: { color: colors.textSecondary }, selected: { color: colors.textPrimary } },
    labelVisibilityMode: 'labeled' as const,
  };
}
