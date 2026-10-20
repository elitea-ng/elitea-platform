import type { BrandTheme } from '@/theme/brandPack';

import { resolveBrand, type BrandInput } from './brand';
import {
  colors as baseColors,
  elevation,
  motion,
  radius,
  size,
  space,
  type Scheme,
  type SemanticColors,
} from './tokens';
import { ACCESSIBILITY_LAYOUT_SCALE, typography, type TypePlatform, type Typography } from './typography';

/** User preference for the colour scheme (Settings → Appearance). */
export type SchemePreference = 'system' | 'light' | 'dark';

/** System accessibility settings that change the tokens. */
export interface AccessibilityModifiers {
  /** iOS Reduce Transparency: glass becomes opaque. */
  reduceTransparency: boolean;
  /** iOS Increase Contrast / Android high-contrast text. */
  increaseContrast: boolean;
  /** Reduce Motion: springs and moves become short cross-fades. */
  reduceMotion: boolean;
  /** Current font scale (1 = default). */
  fontScale: number;
}

export const DEFAULT_A11Y: AccessibilityModifiers = {
  reduceTransparency: false,
  increaseContrast: false,
  reduceMotion: false,
  fontScale: 1,
};

export interface Design {
  scheme: Scheme;
  colors: SemanticColors;
  typography: Typography;
  space: typeof space;
  radius: typeof radius;
  elevation: (typeof elevation)[Scheme];
  size: typeof size;
  motion: typeof motion;
  /** Whether glass surfaces may blur (false: draw `surfaceGlass.opaqueFallback`). */
  glass: boolean;
  reduceMotion: boolean;
  /** Font scale is large enough that horizontal rows should stack. */
  accessibilityLayout: boolean;
  /** Whether a workspace brand changed any colour. */
  branded: boolean;
}

/**
 * The brand inputs of a workspace theme. Reads the pack's unfiltered accent
 * values (`accentSource`) so a weak accent is corrected rather than dropped and
 * a missing scheme is derived from the other one before `brand.hue` (§3.4).
 * A theme persisted before `accentSource` existed falls back to the mapped
 * palette, which the legacy theme still uses.
 */
export function brandInputFrom(theme: BrandTheme | null): BrandInput | null {
  if (!theme) return null;
  const raw = theme.accentSource;
  if (raw) {
    return {
      productDefault: theme.productDefault,
      lightAccent: raw.lightAccent,
      darkAccent: raw.darkAccent,
      hue: raw.hue,
      lightOn: raw.lightOn,
      darkOn: raw.darkOn,
    };
  }
  return {
    productDefault: theme.productDefault,
    lightAccent: theme.light.accent ?? null,
    darkAccent: theme.dark.accent ?? null,
    lightOn: theme.light.onAccent ?? null,
    darkOn: theme.dark.onAccent ?? null,
  };
}

export function schemeFor(preference: SchemePreference, system: string | null | undefined): Scheme {
  if (preference !== 'system') return preference;
  return system === 'dark' ? 'dark' : 'light';
}

function applyContrast(c: SemanticColors, a11y: AccessibilityModifiers): SemanticColors {
  let out = c;
  if (a11y.increaseContrast) {
    out = { ...out, textSecondary: out.textPrimary, textTertiary: out.textSecondary, separator: out.separatorStrong };
  }
  if (a11y.reduceTransparency || a11y.increaseContrast) {
    out = { ...out, surfaceGlass: { ...out.surfaceGlass, rgb: out.surfaceGlass.opaqueFallback, alpha: 1, blur: 0 } };
  }
  return out;
}

export interface ResolveInput {
  scheme: Scheme;
  platform: TypePlatform;
  brand?: BrandInput | null;
  brandFont?: string | null;
  a11y?: AccessibilityModifiers;
}

/** Composes scheme + Zefir tokens + corrected brand overrides + accessibility modifiers. */
export function resolveDesign({ scheme, platform, brand = null, brandFont = null, a11y = DEFAULT_A11Y }: ResolveInput): Design {
  const overrides = resolveBrand(brand)[scheme];
  const branded = Object.keys(overrides).length > 0;
  const colors = applyContrast({ ...baseColors[scheme], ...overrides }, a11y);
  return {
    scheme,
    colors,
    typography: typography(platform, brandFont),
    space,
    radius,
    elevation: elevation[scheme],
    size,
    motion,
    glass: !(a11y.reduceTransparency || a11y.increaseContrast),
    reduceMotion: a11y.reduceMotion,
    accessibilityLayout: a11y.fontScale >= ACCESSIBILITY_LAYOUT_SCALE,
    branded,
  };
}
