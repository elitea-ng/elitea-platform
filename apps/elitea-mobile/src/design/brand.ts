import { BLACK, WHITE, contrastRatio, formatColor, parseColor, type Rgba } from '@/theme/color';

import { accentGrounds, minContrast, rgb } from './contrast';
import { fromOklch, toOklch } from './oklch';
import { BRAND_TARGETS, colors as baseColors, type BrandOverridableKey, type Scheme, type SemanticColors } from './tokens';

/**
 * Workspace brand → Agent Zefir token overrides (docs/design-system.md §3).
 *
 * A brand only *accents* the app: it changes the accent family, the user
 * bubble, the selected chip and a tint of the gradient and orb. Neutrals,
 * status colours and focus never come from a brand. A brand colour that is
 * too weak is not dropped but corrected: its OKLCH lightness moves (darker
 * on light, lighter on dark) until it reaches 4.5:1 on every ground it can
 * appear on, so the brand still shows.
 */

export type BrandOverrides = Partial<Pick<SemanticColors, BrandOverridableKey | 'backgroundGradient' | 'orb'>>;

const STEP = 0.004;
const MAX_STEPS = 300;

/** -1 darkens (light scheme), +1 lightens (dark scheme). */
function direction(scheme: Scheme): -1 | 1 {
  return scheme === 'light' ? -1 : 1;
}

/** Moves `color`'s OKLCH lightness in `dir` until `ok` holds (bounded). */
function pushLightness(color: Rgba, dir: -1 | 1, ok: (c: Rgba) => boolean): Rgba {
  const { l, c, h } = toOklch(color);
  let out = color;
  for (let n = 1; !ok(out) && n <= MAX_STEPS; n++) {
    out = fromOklch({ l: l + dir * STEP * n, c, h });
  }
  return out;
}

/** White or black, whichever contrasts more with `fill`. */
export function bestOn(fill: Rgba): Rgba {
  return contrastRatio(WHITE, fill) >= contrastRatio(BLACK, fill) ? WHITE : BLACK;
}

function pickOn(fill: Rgba, preferred: Rgba | null): Rgba {
  if (preferred && preferred.a === 1 && contrastRatio(preferred, fill) >= BRAND_TARGETS.accentOnAccent) return preferred;
  return bestOn(fill);
}

export interface CorrectedAccent {
  accent: Rgba;
  on: Rgba;
}

/**
 * The brand colour corrected for `scheme`: at least 4.5:1 on every accent
 * ground (§3.3), with a label colour at 4.5:1 on it. `preferredOn` (the pack's
 * button text) wins when it passes. Null for a translucent colour, or when no
 * lightness satisfies both bars.
 */
export function correctAccent(
  brand: Rgba,
  scheme: Scheme,
  preferredOn: Rgba | null = null,
  grounds: readonly Rgba[] = accentGrounds(baseColors[scheme]),
): CorrectedAccent | null {
  if (brand.a < 1) return null;
  const target = BRAND_TARGETS.accentOnBackground;
  let accent = pushLightness(brand, direction(scheme), (c) => minContrast(c, grounds) >= target);
  let on = pickOn(accent, preferredOn);
  // Mid-tone guard: no label reaches 4.5 on the accent, so push it away from the label colour.
  if (contrastRatio(on, accent) < BRAND_TARGETS.accentOnAccent) {
    const dir = on === WHITE ? -1 : 1;
    accent = pushLightness(accent, dir, (c) => contrastRatio(bestOn(c), c) >= BRAND_TARGETS.accentOnAccent);
    on = pickOn(accent, preferredOn);
  }
  if (minContrast(accent, grounds) < target || contrastRatio(on, accent) < BRAND_TARGETS.accentOnAccent) return null;
  return { accent, on };
}

/** Every override for one scheme, derived from one brand colour. */
export function brandOverrides(
  brand: Rgba,
  scheme: Scheme,
  preferredOn: Rgba | null = null,
  base: SemanticColors = baseColors[scheme],
): BrandOverrides | null {
  // Gradient tint: stop 0 takes the brand hue, kept only while tertiary text still reads on it.
  // The accent is then corrected against the tinted stop too (§3.3, "incl. tinted stop").
  const tint = fromOklch({ l: scheme === 'light' ? 0.94 : 0.19, c: 0.03, h: toOklch(brand).h });
  const tinted = contrastRatio(rgb(base.textTertiary), tint) >= BRAND_TARGETS.tintTertiary;
  const grounds = tinted ? [...accentGrounds(base), tint] : accentGrounds(base);
  const corrected = correctAccent(brand, scheme, preferredOn, grounds);
  if (!corrected) return null;
  const { accent, on } = corrected;
  const { l, c, h } = toOklch(accent);
  const dir = direction(scheme);

  // Pressed moves away from the label colour, so the label only gains contrast.
  const pressedDir = contrastRatio(on, BLACK) > contrastRatio(on, WHITE) ? -1 : 1;
  const pressed = fromOklch({ l: l + pressedDir * 0.06, c, h });

  const soft = fromOklch({ l: scheme === 'light' ? 0.94 : 0.27, c: Math.min(c, 0.045), h });
  const softOn = pushLightness(
    fromOklch({ l: scheme === 'light' ? 0.45 : 0.85, c, h }),
    dir,
    (x) => contrastRatio(x, soft) >= BRAND_TARGETS.accentOnAccent,
  );
  const bubble = scheme === 'light' ? accent : fromOklch({ l: 0.33, c: Math.min(c, 0.1), h });

  const out: BrandOverrides = {
    accent: formatColor(accent),
    accentPressed: formatColor(pressed),
    accentOn: formatColor(on),
    accentSoft: formatColor(soft),
    accentSoftOn: formatColor(softOn),
    bubbleUser: formatColor(bubble),
    bubbleUserText: formatColor(bestOn(bubble)),
    chipSelected: formatColor(soft),
    chipSelectedText: formatColor(softOn),
  };

  if (tinted) {
    const [, s1, s2] = base.backgroundGradient.stops;
    out.backgroundGradient = { ...base.backgroundGradient, stops: [formatColor(tint), s1, s2] };
  }

  // Orb tint (decorative): stop 0 at its own lightness and chroma, in the brand hue.
  const orb0 = toOklch(rgb(base.orb.stops[0]));
  const [, o1, o2] = base.orb.stops;
  out.orb = { ...base.orb, stops: [formatColor(fromOklch({ l: orb0.l, c: orb0.c, h })), o1, o2] };
  return out;
}

/** The brand inputs the app has for a workspace. All optional; colours are pack strings. */
export interface BrandInput {
  /** `productDefault`: the server serves its product default, i.e. has no brand of its own. */
  productDefault: boolean;
  lightAccent?: string | null;
  darkAccent?: string | null;
  /** `brand.hue` of the pack. */
  hue?: string | null;
  lightOn?: string | null;
  darkOn?: string | null;
}

function solid(value: string | null | undefined): Rgba | null {
  const color = parseColor(value);
  return color && color.a === 1 ? color : null;
}

/**
 * Overrides for both schemes (§3.4): each scheme reads its own accent; when
 * only one scheme has one, the other is derived from it; else `hue`; else
 * nothing (Zefir rose stays). `productDefault` means no colour override at all.
 */
export function resolveBrand(input: BrandInput | null): Record<Scheme, BrandOverrides> {
  const none = { light: {}, dark: {} };
  if (!input || input.productDefault) return none;
  const light = solid(input.lightAccent);
  const dark = solid(input.darkAccent);
  const hue = solid(input.hue);
  const source: Record<Scheme, Rgba | null> = { light: light ?? dark ?? hue, dark: dark ?? light ?? hue };
  const on: Record<Scheme, Rgba | null> = { light: solid(input.lightOn), dark: solid(input.darkOn) };
  const out: Record<Scheme, BrandOverrides> = { light: {}, dark: {} };
  for (const scheme of ['light', 'dark'] as const) {
    const color = source[scheme];
    // The pack's label colour only applies to the scheme's own accent, not a borrowed one.
    const ownAccent = scheme === 'light' ? light : dark;
    if (color) out[scheme] = brandOverrides(color, scheme, ownAccent ? on[scheme] : null) ?? {};
  }
  return out;
}
