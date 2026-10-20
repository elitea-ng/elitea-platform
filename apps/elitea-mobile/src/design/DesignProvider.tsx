import { createContext, useContext, useMemo, useSyncExternalStore, type ReactNode } from 'react';
import { AccessibilityInfo, Platform, useColorScheme, useWindowDimensions } from 'react-native';

import { useBrandFontFamily } from '@/theme/brandFonts';
import { useBrandTheme } from '@/theme/brandStore';

import type { BrandInput } from './brand';
import {
  brandInputFrom,
  DEFAULT_A11Y,
  resolveDesign,
  schemeFor,
  type AccessibilityModifiers,
  type Design,
  type SchemePreference,
} from './resolve';

/**
 * The design system at runtime: `useDesign()` returns the resolved tokens for
 * the current scheme, the connected workspace's corrected brand accent, and
 * the system accessibility settings.
 *
 * Screens adopt it one by one. `<DesignProvider>` is optional: it lets a
 * subtree force a scheme (Settings → Appearance) and shares one subscription
 * to the accessibility settings; without it `useDesign()` resolves the same
 * design from the system scheme on its own.
 */

const DesignContext = createContext<Design | null>(null);

const BOOL_SETTINGS = [
  ['reduceTransparency', 'reduceTransparencyChanged', () => AccessibilityInfo.isReduceTransparencyEnabled()],
  ['reduceMotion', 'reduceMotionChanged', () => AccessibilityInfo.isReduceMotionEnabled()],
  [
    'increaseContrast',
    Platform.OS === 'android' ? 'highTextContrastChanged' : 'darkerSystemColorsChanged',
    () =>
      Platform.OS === 'android'
        ? AccessibilityInfo.isHighTextContrastEnabled()
        : AccessibilityInfo.isDarkerSystemColorsEnabled(),
  ],
] as const;

type Flag = (typeof BOOL_SETTINGS)[number][0];
type Flags = Record<Flag, boolean>;

/**
 * One app-wide subscription to the accessibility settings, started by the
 * first component that reads them and stopped when the last one unmounts.
 */
let flags: Flags = {
  reduceTransparency: DEFAULT_A11Y.reduceTransparency,
  reduceMotion: DEFAULT_A11Y.reduceMotion,
  increaseContrast: DEFAULT_A11Y.increaseContrast,
};
const listeners = new Set<() => void>();
let stop: (() => void) | null = null;

function setFlag(key: Flag, value: boolean) {
  if (flags[key] === value) return;
  flags = { ...flags, [key]: value };
  for (const listener of [...listeners]) listener();
}

function start(): () => void {
  let active = true;
  const subscriptions = BOOL_SETTINGS.map(([key, event, read]) => {
    // Not every query exists on every platform; a missing one keeps the default.
    try {
      read()
        .then((value) => active && setFlag(key, value === true))
        .catch(() => {});
      return AccessibilityInfo.addEventListener(event, (value: unknown) => setFlag(key, value === true));
    } catch {
      return null;
    }
  });
  return () => {
    active = false;
    for (const subscription of subscriptions) subscription?.remove();
  };
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  if (stop === null) stop = start();
  return () => {
    listeners.delete(listener);
    if (listeners.size === 0 && stop !== null) {
      stop();
      stop = null;
    }
  };
}

const getFlags = () => flags;

/** The system accessibility settings that change tokens, kept current. */
export function useAccessibilityModifiers(): AccessibilityModifiers {
  const { fontScale } = useWindowDimensions();
  const current = useSyncExternalStore(subscribe, getFlags, getFlags);
  return useMemo(() => ({ ...current, fontScale: fontScale || 1 }), [current, fontScale]);
}

function useResolvedDesign(preference: SchemePreference, a11y: AccessibilityModifiers, brandOverride?: BrandInput | null): Design {
  const system = useColorScheme();
  const brand = useBrandTheme();
  const brandFont = useBrandFontFamily() ?? null;
  const scheme = schemeFor(preference, system);
  const brandInput = useMemo(() => (brandOverride !== undefined ? brandOverride : brandInputFrom(brand)), [brandOverride, brand]);
  return useMemo(
    () =>
      resolveDesign({
        scheme,
        platform: Platform.OS === 'ios' ? 'ios' : 'android',
        brand: brandInput,
        brandFont: brandOverride !== undefined ? null : brandFont,
        a11y,
      }),
    [scheme, brandInput, brandFont, brandOverride, a11y],
  );
}

/**
 * Provides the design to a subtree. The app root mounts one with the system scheme (Settings →
 * Appearance applies its choice through `Appearance.setColorScheme`, src/state/appearance.ts).
 * `scheme` forces a scheme for a subtree; `brand` replaces the workspace brand for a subtree
 * (`null`: unbranded Zefir), which the design gallery uses to show a branded preview.
 */
export function DesignProvider({
  scheme = 'system',
  brand,
  children,
}: {
  scheme?: SchemePreference;
  brand?: BrandInput | null;
  children: ReactNode;
}) {
  const a11y = useAccessibilityModifiers();
  const design = useResolvedDesign(scheme, a11y, brand);
  return <DesignContext.Provider value={design}>{children}</DesignContext.Provider>;
}

/** The resolved design for this subtree. */
export function useDesign(): Design {
  const provided = useContext(DesignContext);
  const a11y = useAccessibilityModifiers();
  const local = useResolvedDesign('system', a11y);
  return provided ?? local;
}
