/**
 * How strongly the sidebar tints the window's native material (macOS
 * vibrancy) with the theme's secondary surface.
 *
 * The material shows the desktop through a blur, so the backdrop of the
 * sidebar's text is unknown. The tint is chosen so the sidebar text keeps
 * WCAG AA (4.5:1) even over the WORST backdrop — black behind the light
 * theme, white behind the dark — with the default brand pack's tokens
 * (`sidebarTint.test.ts` computes it). With the window's appearance now
 * following the app's mode (`useNativeWindowTheme`), the real material is of
 * the same lightness as the surface and the margin is larger still. At 0.55
 * the dark sidebar over a light material measured 2.4:1.
 */
export const SIDEBAR_TINT_OPACITY = 0.88;

type Rgb = readonly [number, number, number];

export function parseHex(hex: string): Rgb {
  const value = /^#([0-9a-f]{6})$/i.exec(hex)?.[1];
  if (value === undefined) throw new Error(`not a #rrggbb colour: ${hex}`);
  return [0, 2, 4].map((i) => Number.parseInt(value.slice(i, i + 2), 16)) as unknown as Rgb;
}

/** `top` at `alpha` over an opaque `bottom`. */
export function composite(top: Rgb, alpha: number, bottom: Rgb): Rgb {
  return top.map((channel, i) => channel * alpha + (bottom[i] ?? 0) * (1 - alpha)) as unknown as Rgb;
}

function luminance(rgb: Rgb): number {
  const [r, g, b] = rgb.map((channel) => {
    const c = channel / 255;
    return c <= 0.039_28 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4;
  }) as unknown as Rgb;
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
}

/** WCAG 2.x contrast ratio. */
export function contrastRatio(a: Rgb, b: Rgb): number {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x) as [number, number];
  return (hi + 0.05) / (lo + 0.05);
}
