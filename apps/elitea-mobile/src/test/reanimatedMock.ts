/**
 * A jest stand-in for the react-native-reanimated surface the app uses.
 *
 * The library's own `react-native-reanimated/mock` imports the real module
 * (`src/index.ts`), whose initializer calls into a native runtime that jest
 * does not have (`setCSSEventHandler` is not available in JSReanimated, on
 * Reanimated 4.7 with react-native 0.88). This stand-in keeps the API shape:
 * shared values are plain objects, animated styles are computed once from
 * their current values, and the animation builders return their target.
 * Extend it when a screen uses more of the API.
 */
import { Image, ScrollView, Text, View } from 'react-native';

type Factory<T> = () => T;

const identityEasing = (value: number) => value;
const easingFactory = () => identityEasing;

export const Easing = {
  linear: identityEasing,
  quad: identityEasing,
  cubic: identityEasing,
  ease: identityEasing,
  in: easingFactory,
  out: easingFactory,
  inOut: easingFactory,
  bezier: () => identityEasing,
};

export function useSharedValue<T>(initial: T): { value: T } {
  return { value: initial };
}

export function useAnimatedStyle<T>(factory: Factory<T>): T {
  return factory();
}

export function useDerivedValue<T>(factory: Factory<T>): { value: T } {
  return { value: factory() };
}

export const withTiming = <T>(value: T): T => value;
export const withSpring = <T>(value: T): T => value;
export const withDelay = <T>(_delay: number, value: T): T => value;
export const withRepeat = <T>(value: T): T => value;
export const withSequence = <T>(...values: T[]): T => values[values.length - 1] as T;
export const cancelAnimation = (): void => {};
export const runOnJS = <A extends unknown[], R>(fn: (...args: A) => R) => fn;
export const interpolate = (value: number, input: number[], output: number[]): number => {
  const [i0 = 0, i1 = 1] = input;
  const [o0 = 0, o1 = 1] = output;
  return i1 === i0 ? o0 : o0 + ((value - i0) / (i1 - i0)) * (o1 - o0);
};
export const useReducedMotion = (): boolean => false;

const Animated = {
  View,
  Text,
  Image,
  ScrollView,
  createAnimatedComponent: <C>(component: C): C => component,
};

export default Animated;
