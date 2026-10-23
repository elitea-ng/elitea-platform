/**
 * jest setup for the native UI modules of the design kit (src/ui/). Tests render screens without
 * the native views, so each module gets its documented jest mock or a plain stand-in:
 *
 * - react-native-keyboard-controller: the library's own jest mock (keyboard hidden);
 * - react-native-safe-area-context: the library's own mock (zero insets, providers);
 * - expo-symbols: a View carrying the symbol names as props, so a test can read which icon a
 *   control draws (`UNSAFE_getByProps({ symbolIos: 'bell' })`) on either platform;
 * - expo-glass-effect: no Liquid Glass (the kit draws its tinted fallback);
 * - expo-haptics: no-ops a test can assert on.
 * A test file that mocks one of these itself still wins.
 */
jest.mock('react-native-keyboard-controller', () => require('react-native-keyboard-controller/jest'));
jest.mock('react-native-safe-area-context', () => require('react-native-safe-area-context/jest/mock').default);
jest.mock('expo-symbols', () => {
  const { createElement } = require('react');
  const { View } = require('react-native');
  return {
    SymbolView: ({ name, ...props }: { name: string | { ios?: string; android?: string } }) =>
      createElement(View, {
        ...props,
        symbolIos: typeof name === 'string' ? name : name.ios,
        symbolAndroid: typeof name === 'string' ? undefined : name.android,
      }),
  };
});
jest.mock('expo-glass-effect', () => {
  const { View } = require('react-native');
  return { GlassView: View, GlassContainer: View, isLiquidGlassAvailable: () => false, isGlassEffectAPIAvailable: () => false };
});
jest.mock('expo-haptics', () => ({
  selectionAsync: jest.fn(async () => {}),
  impactAsync: jest.fn(async () => {}),
  notificationAsync: jest.fn(async () => {}),
  performAndroidHapticsAsync: jest.fn(async () => {}),
  ImpactFeedbackStyle: { Light: 'light', Medium: 'medium', Heavy: 'heavy', Rigid: 'rigid', Soft: 'soft' },
  NotificationFeedbackType: { Success: 'success', Warning: 'warning', Error: 'error' },
  AndroidHaptics: { Confirm: 'confirm', Reject: 'reject' },
}));
