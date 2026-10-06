/**
 * Dynamic Type limits (`maxFontSizeMultiplier`). Text follows the system
 * text size, up to these caps, so the largest accessibility sizes stay
 * readable without breaking fixed-height rows:
 *
 * - `body`: message text and notes: grows the most;
 * - `input`: the composer and other text fields;
 * - `control`: buttons, action rows, chips (they sit in rows of fixed height);
 * - `chrome`: small labels in tight places (badges, section titles).
 */
export const FONT_SCALE = {
  body: 3,
  input: 2,
  control: 1.6,
  chrome: 1.35,
} as const;
