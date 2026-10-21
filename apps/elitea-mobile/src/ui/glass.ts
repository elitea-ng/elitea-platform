import type { Glass } from '@/design/tokens';

import type { UiPlatform } from './platform';

/** How a glass surface renders (docs/design-system.md §1.3). */
export type GlassMode = 'liquid' | 'tinted' | 'opaque';

/**
 * - iOS 26+ with transparency allowed: the system Liquid Glass material.
 * - Android, and iOS before 26: a tonal surface, the glass colour at α .92 (no real-time blur).
 * - Reduce Transparency / Increase Contrast: the opaque fallback.
 */
export function glassMode(on: UiPlatform, liquidAvailable: boolean, glassAllowed: boolean): GlassMode {
  if (!glassAllowed) return 'opaque';
  if (on === 'ios' && liquidAvailable) return 'liquid';
  return 'tinted';
}

/** `#RRGGBB` at an alpha, as `rgba()`. */
export function withAlpha(hex: string, alpha: number): string {
  const m = /^#([0-9a-f]{2})([0-9a-f]{2})([0-9a-f]{2})$/i.exec(hex);
  if (!m) return hex;
  return `rgba(${parseInt(m[1], 16)}, ${parseInt(m[2], 16)}, ${parseInt(m[3], 16)}, ${alpha})`;
}

/** The fill a non-liquid glass surface draws. */
export function glassFill(glass: Glass, mode: GlassMode): string {
  if (mode === 'opaque') return glass.opaqueFallback;
  return withAlpha(glass.opaqueFallback, 0.92);
}
