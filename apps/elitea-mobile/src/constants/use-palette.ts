import { useMemo } from 'react';

import { useDesign } from '@/design/DesignProvider';
import type { SemanticColors } from '@/design/tokens';

import { Colors, type Palette } from './theme';

/**
 * The current palette, now a view of the design system (docs/design-system.md §6.1): the Zefir
 * semantic tokens of the active scheme with the workspace's CORRECTED brand accent on top
 * (`useDesign()`, src/design/). The pack's web neutrals no longer reach the app (§3.2), and a
 * weak brand accent is corrected instead of dropped (§3.3).
 *
 * New code reads `useDesign()` and the kit (src/ui/) directly; this keeps the screens that
 * still speak the eight legacy names on the same tokens until each one moves over.
 */
export function usePalette(): AppPalette {
  const { colors } = useDesign();
  return useMemo(() => paletteFromDesign(colors), [colors]);
}

/** The legacy names plus the two that inputs need (a 3:1 border, the sunken field fill). */
export type AppPalette = Palette & { inputBorder: string; field: string };

export function paletteFromDesign(c: SemanticColors): AppPalette {
  return {
    text: c.textPrimary,
    textSecondary: c.textSecondary,
    background: c.background,
    backgroundElement: c.surface,
    border: c.separator,
    accent: c.accent,
    onAccent: c.accentOn,
    danger: c.danger,
    inputBorder: c.separatorStrong,
    field: c.surfaceSunken,
  };
}

/**
 * The legacy brand-pack merge (fallback colours + the pack's vetted overrides). Kept for the
 * brand store's own tests of what the pack mapper produces; no screen renders with it.
 */
export function resolvePalette(scheme: 'light' | 'dark', overrides: Partial<Palette> | undefined): Palette {
  return overrides ? { ...Colors[scheme], ...overrides } : Colors[scheme];
}
