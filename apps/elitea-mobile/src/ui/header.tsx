import type { NativeStackHeaderItem, NativeStackNavigationOptions } from 'expo-router';
import { Platform, StyleSheet, View } from 'react-native';

import type { Design } from '@/design/resolve';

import { badgeText } from './Badge';
import { IconButton } from './IconButton';
import { ICONS, type IconName } from './icons';
import { uiPlatform, type UiPlatform } from './platform';

/** One action in a navigation bar / top app bar. */
export interface HeaderAction {
  id: string;
  icon: IconName;
  /** What it does ("New chat"); also the accessibility label. */
  label: string;
  onPress: () => void;
  /** A count (notifications): a native bar-button badge on iOS 26+, an in-frame badge elsewhere. */
  badge?: number;
  /** The full spoken label when it says more than `label` ("Notifications, 3 unread"). */
  accessibilityLabel?: string;
  disabled?: boolean;
  /**
   * Work it started is running (an export): read as busy. The kit button sets
   * `accessibilityState.busy`; a native bar button has no busy state, so its spoken label says
   * "in progress" instead (F3).
   */
  busy?: boolean;
}

/** The spoken label of a native bar button: its label, plus "in progress" while busy. */
export function nativeHeaderItemLabel(action: Pick<HeaderAction, 'label' | 'accessibilityLabel' | 'busy'>): string {
  const base = action.accessibilityLabel ?? action.label;
  return action.busy ? `${base}, in progress` : base;
}

/** The iOS major version (`Platform.Version` is "26.0" on iOS), 0 elsewhere. */
export function iosMajor(): number {
  if (Platform.OS !== 'ios') return 0;
  const major = parseInt(String(Platform.Version), 10);
  return Number.isFinite(major) ? major : 0;
}

/**
 * iOS: native bar button items (`unstable_headerRightItems`), so iOS 26 draws them as one Liquid
 * Glass group with SF Symbols and system badges (X5, X6: no custom border inside the glass, no
 * badge at a negative offset that the capsule clips). Below iOS 26 bar buttons have no badge,
 * so the items render as kit icon buttons with the badge inside their frame.
 *
 * Android: Material 3 top-app-bar action icons (48 dp, ripple), at most three (more belong in
 * an overflow menu).
 */
export function headerActionOptions(
  actions: readonly HeaderAction[],
  design: Pick<Design, 'colors'>,
  on: UiPlatform = uiPlatform,
  major: number = iosMajor(),
): Pick<NativeStackNavigationOptions, 'headerRight' | 'unstable_headerRightItems'> {
  if (actions.length === 0) return {};
  if (on === 'ios' && major >= 26) {
    return {
      unstable_headerRightItems: () =>
        actions.map(
          (action): NativeStackHeaderItem => ({
            type: 'button',
            label: action.label,
            icon: { type: 'sfSymbol', name: ICONS[action.icon][0] },
            onPress: action.onPress,
            disabled: action.disabled,
            identifier: action.id,
            tintColor: design.colors.textPrimary,
            accessibilityLabel: nativeHeaderItemLabel(action),
            badge:
              action.badge !== undefined && action.badge > 0
                ? { value: badgeText(action.badge), style: { backgroundColor: design.colors.badge, color: design.colors.badgeText } }
                : undefined,
          }),
        ),
    };
  }
  return {
    headerRight: () => (
      <View style={styles.actions}>
        {actions.slice(0, 3).map((action) => (
          <IconButton
            key={action.id}
            icon={action.icon}
            variant="plain"
            size={on === 'android' ? 48 : 36}
            accessibilityLabel={action.accessibilityLabel ?? action.label}
            badge={action.badge}
            disabled={action.disabled}
            accessibilityState={action.busy ? { busy: true } : undefined}
            onPress={action.onPress}
          />
        ))}
      </View>
    ),
  };
}

/**
 * A top-level list screen (Chats, Discover, Settings): the iOS large title that collapses into
 * the inline title on scroll. The bar keeps the system background (iOS 26: transparent with the
 * scroll-edge effect), and the scroll view adjusts its content inset under it. Android draws its M3 top app bar.
 */
export function largeTitleOptions(design: Pick<Design, 'colors'>): NativeStackNavigationOptions {
  return {
    headerLargeTitle: true,
    headerLargeTitleShadowVisible: false,
    headerShadowVisible: false,
    headerTintColor: design.colors.accent,
    headerLargeTitleStyle: { color: design.colors.textPrimary },
    headerTitleStyle: { color: design.colors.textPrimary },
  };
}

const styles = StyleSheet.create({
  actions: { flexDirection: 'row', alignItems: 'center', gap: 4 },
});

/**
 * A native search field in the navigation bar (iOS `UISearchController`, which iOS 26 may move
 * into the bottom toolbar; on Android the header search view of react-native-screens).
 */
export function searchBarOptions(
  design: Pick<Design, 'colors'>,
  { placeholder, onChange, onCancel }: { placeholder: string; onChange: (text: string) => void; onCancel?: () => void },
): Pick<NativeStackNavigationOptions, 'headerSearchBarOptions'> {
  return {
    headerSearchBarOptions: {
      placeholder,
      hideWhenScrolling: false,
      autoCapitalize: 'none',
      tintColor: design.colors.accent,
      textColor: design.colors.textPrimary,
      hintTextColor: design.colors.textTertiary,
      headerIconColor: design.colors.textPrimary,
      onChangeText: (event) => onChange(event.nativeEvent.text),
      onCancelButtonPress: () => {
        onChange('');
        onCancel?.();
      },
      onClose: () => onChange(''),
    },
  };
}
