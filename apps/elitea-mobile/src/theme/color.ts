/**
 * Strict colour parsing for server-supplied brand values. Only plain colours
 * are accepted: `#rgb`, `#rrggbb`, `#rrggbbaa`, `rgb(r, g, b)` and
 * `rgba(r, g, b, a)` with decimal channels. Everything else (gradients,
 * `url()`, `var()`, `expression()`, named colours, hsl) is refused, so a pack
 * can never smuggle anything but a colour into a style.
 */

export interface Rgba {
  r: number;
  g: number;
  b: number;
  /** 0..1 */
  a: number;
}

const HEX = /^#([0-9a-f]{3}|[0-9a-f]{6}|[0-9a-f]{8})$/i;
const RGB = /^rgba?\(\s*(\d{1,3})\s*,\s*(\d{1,3})\s*,\s*(\d{1,3})\s*(?:,\s*(\d*\.?\d+)\s*)?\)$/i;

export function parseColor(value: unknown): Rgba | null {
  if (typeof value !== 'string' || value.length > 64) return null;
  const text = value.trim();
  const hex = HEX.exec(text);
  if (hex) {
    let digits = hex[1];
    if (digits.length === 3) digits = digits.replace(/./g, (d) => d + d);
    const n = (i: number) => parseInt(digits.slice(i, i + 2), 16);
    return { r: n(0), g: n(2), b: n(4), a: digits.length === 8 ? round(n(6) / 255) : 1 };
  }
  const rgb = RGB.exec(text);
  if (rgb) {
    const isRgba = text.toLowerCase().startsWith('rgba');
    if (isRgba !== (rgb[4] !== undefined)) return null; // rgb() with alpha or rgba() without
    const [r, g, b] = [rgb[1], rgb[2], rgb[3]].map(Number);
    const a = rgb[4] === undefined ? 1 : Number(rgb[4]);
    if ([r, g, b].some((c) => c > 255) || !(a >= 0 && a <= 1)) return null;
    return { r, g, b, a };
  }
  return null;
}

function round(n: number): number {
  return Math.round(n * 1000) / 1000;
}

/** A React Native colour string: `#rrggbb` when opaque, else `rgba(...)`. */
export function formatColor({ r, g, b, a }: Rgba): string {
  if (a >= 1) return `#${[r, g, b].map((c) => c.toString(16).padStart(2, '0')).join('')}`;
  return `rgba(${r}, ${g}, ${b}, ${round(a)})`;
}

/** `top` alpha-composited over an opaque `bottom`. */
export function composite(top: Rgba, bottom: Rgba): Rgba {
  const mix = (t: number, b: number) => Math.round(t * top.a + b * (1 - top.a));
  return { r: mix(top.r, bottom.r), g: mix(top.g, bottom.g), b: mix(top.b, bottom.b), a: 1 };
}

function channel(c: number): number {
  const s = c / 255;
  return s <= 0.03928 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4;
}

/** WCAG 2.x relative luminance of an opaque colour. */
export function luminance({ r, g, b }: Rgba): number {
  return 0.2126 * channel(r) + 0.7152 * channel(g) + 0.0722 * channel(b);
}

/** WCAG contrast ratio of `fg` (composited over `bg`) against opaque `bg`. */
export function contrastRatio(fg: Rgba, bg: Rgba): number {
  const front = fg.a < 1 ? composite(fg, bg) : fg;
  const l1 = luminance(front);
  const l2 = luminance(bg);
  const [hi, lo] = l1 > l2 ? [l1, l2] : [l2, l1];
  return (hi + 0.05) / (lo + 0.05);
}

export const BLACK: Rgba = { r: 0, g: 0, b: 0, a: 1 };
export const WHITE: Rgba = { r: 255, g: 255, b: 255, a: 1 };
