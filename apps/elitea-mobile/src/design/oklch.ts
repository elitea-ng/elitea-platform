import type { Rgba } from '@/theme/color';

/**
 * sRGB ↔ OKLCH (Björn Ottosson's OKLab, polar form), used by the brand
 * contrast correction to move a colour's lightness while keeping its hue.
 * Mirrors the reference implementation the design proofs were computed with.
 */

export interface Oklch {
  /** Lightness 0..1. */
  l: number;
  /** Chroma, >= 0 (sRGB colours stay under ~0.33). */
  c: number;
  /** Hue in radians. */
  h: number;
}

function toLinear(c: number): number {
  const s = c / 255;
  return s <= 0.04045 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4;
}

function fromLinear(c: number): number {
  const s = c <= 0.0031308 ? 12.92 * c : 1.055 * c ** (1 / 2.4) - 0.055;
  return Math.max(0, Math.min(255, Math.round(s * 255)));
}

export function toOklch({ r, g, b }: Rgba): Oklch {
  const [lr, lg, lb] = [r, g, b].map(toLinear);
  const l = Math.cbrt(0.4122214708 * lr + 0.5363325363 * lg + 0.0514459929 * lb);
  const m = Math.cbrt(0.2119034982 * lr + 0.6806995451 * lg + 0.1073969566 * lb);
  const s = Math.cbrt(0.0883024619 * lr + 0.2817188376 * lg + 0.6299787005 * lb);
  const L = 0.2104542553 * l + 0.793617785 * m - 0.0040720468 * s;
  const A = 1.9779984951 * l - 2.428592205 * m + 0.4505937099 * s;
  const B = 0.0259040371 * l + 0.7827717662 * m - 0.808675766 * s;
  return { l: L, c: Math.hypot(A, B), h: Math.atan2(B, A) };
}

const EPS = 1e-4;

/**
 * The opaque sRGB colour for `lch`. Out-of-gamut values are mapped into sRGB
 * by reducing chroma (5% per step), so the hue and lightness survive.
 */
export function fromOklch({ l: L, c, h }: Oklch): Rgba {
  let C = c;
  let rgb: [number, number, number] = [0, 0, 0];
  for (let i = 0; i < 40; i++) {
    const A = C * Math.cos(h);
    const B = C * Math.sin(h);
    const l = (L + 0.3963377774 * A + 0.2158037573 * B) ** 3;
    const m = (L - 0.1055613458 * A - 0.0638541728 * B) ** 3;
    const s = (L - 0.0894841775 * A - 1.291485548 * B) ** 3;
    rgb = [
      4.0767416621 * l - 3.3077115913 * m + 0.2309699292 * s,
      -1.2684380046 * l + 2.6097574011 * m - 0.3413193965 * s,
      -0.0041960863 * l - 0.7034186147 * m + 1.707614701 * s,
    ];
    if (rgb.every((x) => x >= -EPS && x <= 1 + EPS)) break;
    C *= 0.95;
  }
  const [r, g, b] = rgb.map((x) => fromLinear(Math.min(1, Math.max(0, x))));
  return { r, g, b, a: 1 };
}
