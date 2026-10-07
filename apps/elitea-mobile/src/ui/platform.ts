import { Platform, type Insets, type PressableAndroidRippleConfig } from 'react-native';

import { size } from '@/design/tokens';
import type { Design } from '@/design/resolve';

export type UiPlatform = 'ios' | 'android';

export const uiPlatform: UiPlatform = Platform.OS === 'ios' ? 'ios' : 'android';

/** Minimum touch target: 44 pt (HIG) on iOS, 48 dp (Material 3) on Android. */
export function hitTarget(on: UiPlatform = uiPlatform): number {
  return size.hitTarget[on];
}

/**
 * `hitSlop` that grows a control drawn at `visual` × `visual` to the platform's minimum
 * target, so a 36 pt circle still takes a 44/48 tap.
 */
export function hitSlopFor(visual: number, on: UiPlatform = uiPlatform): Insets | undefined {
  const missing = hitTarget(on) - visual;
  if (missing <= 0) return undefined;
  const each = Math.ceil(missing / 2);
  return { top: each, bottom: each, left: each, right: each };
}

/** The pressed overlay (§2.2): textPrimary α .08 in light, white α .10 in dark. */
export function pressedOverlay(design: Pick<Design, 'scheme'>): string {
  return design.scheme === 'dark' ? 'rgba(255, 255, 255, 0.10)' : 'rgba(28, 22, 34, 0.08)';
}

/** Android ripple in the pressed-overlay colour; iOS draws its own highlight instead. */
export function ripple(design: Pick<Design, 'scheme'>, borderless = false): PressableAndroidRippleConfig {
  return { color: pressedOverlay(design), borderless, foreground: true };
}
