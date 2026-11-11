/**
 * jest setup for the native UI modules of the design kit (src/ui/). Tests render screens without
 * the native views, so each module gets its documented jest mock or a plain stand-in:
 *
 * - react-native-keyboard-controller: the library's own jest mock (keyboard hidden);
 * - react-native-safe-area-context: the library's own mock (zero insets, providers);
 * - expo-symbols: a View carrying the symbol names as props, so a test can read which icon a
 *   control draws (`UNSAFE_getByProps({ symbolIos: 'bell' })`) on either platform;
 * - expo-glass-effect: no Liquid Glass (the kit draws its tinted fallback);
 * - expo-haptics: no-ops a test can assert on;
 * - react-native-reanimated: `src/test/reanimatedMock.ts` (the library's own mock loads the native
 *   runtime and throws under jest; animated styles resolve to their first frame);
 * - expo-image and expo-video: Views carrying their props, and a fake video player (avatar packs).
 * A test file that mocks one of these itself still wins.
 */
jest.mock('react-native-reanimated', () => require('./reanimatedMock'));
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
// expo-image: a View carrying the props, so a test reads `source.uri` and `autoplay`.
jest.mock('expo-image', () => {
  const { createElement } = require('react');
  const { View } = require('react-native');
  return { Image: (props: Record<string, unknown>) => createElement(View, props) };
});
// expo-video: a View carrying the props, and a player a test can read (`loop`, `muted`, `playing`)
// and drive (`player.emit('statusChange', { status: 'error' })`).
jest.mock('expo-video', () => {
  const { createElement, useState } = require('react');
  const { View } = require('react-native');
  const createPlayer = (source: unknown) => {
    const listeners = new Map<string, Set<(payload: unknown) => void>>();
    return {
      source,
      loop: false,
      muted: false,
      playing: false,
      status: 'idle',
      play() {
        this.playing = true;
      },
      pause() {
        this.playing = false;
      },
      release() {},
      addListener(event: string, listener: (payload: unknown) => void) {
        if (!listeners.has(event)) listeners.set(event, new Set());
        listeners.get(event)?.add(listener);
        return { remove: () => listeners.get(event)?.delete(listener) };
      },
      emit(event: string, payload: unknown) {
        for (const listener of listeners.get(event) ?? []) listener(payload);
      },
    };
  };
  return {
    useVideoPlayer: (source: unknown, setup?: (player: unknown) => void) => {
      const [player] = useState(() => {
        const created = createPlayer(source);
        setup?.(created);
        return created;
      });
      return player;
    },
    VideoView: (props: Record<string, unknown>) => createElement(View, props),
  };
});
