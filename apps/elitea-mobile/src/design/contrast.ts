import { BLACK, composite, contrastRatio, parseColor, WHITE, type Rgba } from '@/theme/color';

import type { ColorKey, ProofGround, SemanticColors } from './tokens';

/** Parses a token colour; tokens are authored, so a bad one is a programming error. */
export function rgb(value: string): Rgba {
  const parsed = parseColor(value);
  if (!parsed) throw new Error(`not a colour token: ${value}`);
  return parsed;
}

/** The opaque colours a glass surface can end up as: its fill over every stop and the flat background. */
export function glassComposites(colors: SemanticColors): Rgba[] {
  const glass = { ...rgb(colors.surfaceGlass.rgb), a: colors.surfaceGlass.alpha };
  return [...colors.backgroundGradient.stops, colors.background].map((under) => composite(glass, rgb(under)));
}

/**
 * Every opaque colour accent-coloured content may sit on: the gradient stops,
 * the flat background, surface, elevated, and glass over each of the grounds.
 */
export function accentGrounds(colors: SemanticColors): Rgba[] {
  return [
    ...colors.backgroundGradient.stops.map(rgb),
    rgb(colors.background),
    rgb(colors.surface),
    rgb(colors.elevated),
    ...glassComposites(colors),
  ];
}

/** Worst-case contrast of `fg` over every colour in `grounds`. */
export function minContrast(fg: Rgba, grounds: readonly Rgba[]): number {
  return Math.min(...grounds.map((ground) => contrastRatio(fg, ground)));
}

/** How strongly a file chip veils the user's own bubble (and, doubled, its file tile). */
export const BUBBLE_CHIP_ALPHA = 0.12;

/**
 * The veil a file chip lays over the user's own bubble. It always moves the bubble away from
 * the bubble's text colour (black under white text, white under black text), so the chip's
 * name and caption never read worse than the bubble's own text, whatever the brand.
 */
export function bubbleChipVeil(colors: SemanticColors, alpha: number = BUBBLE_CHIP_ALPHA): Rgba {
  const text = rgb(colors.bubbleUserText);
  const ink = contrastRatio(text, BLACK) >= contrastRatio(text, WHITE) ? BLACK : WHITE;
  return { ...ink, a: alpha };
}

/** The opaque colour of a file chip on the user's own bubble. */
export function bubbleChipGround(colors: SemanticColors): Rgba {
  return composite(bubbleChipVeil(colors), rgb(colors.bubbleUser));
}

/** The grounds a proof's background key stands for. */
export function proofGrounds(colors: SemanticColors, key: ProofGround): Rgba[] {
  if (key === 'backgroundGradient') return colors.backgroundGradient.stops.map(rgb);
  if (key === 'surfaceGlass') return glassComposites(colors);
  if (key === 'bubbleUserChip') return [bubbleChipGround(colors)];
  return [rgb(colors[key])];
}

/** Worst-case ratio of one proof pair. */
export function proofRatio(
  colors: SemanticColors,
  fg: ColorKey,
  bg: ProofGround,
): number {
  return minContrast(rgb(colors[fg]), proofGrounds(colors, bg));
}
