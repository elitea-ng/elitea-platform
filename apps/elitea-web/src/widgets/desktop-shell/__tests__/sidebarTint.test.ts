import { describe, expect, it } from 'vitest';

import pack from '@/shared/brand/tokens/default.pack.json';

import { composite, contrastRatio, parseHex, SIDEBAR_TINT_OPACITY } from '../lib/sidebarTint';

const schemes = pack.schemes as unknown as Record<'light' | 'dark', Record<string, string>>;
const BLACK = [0, 0, 0] as const;
const WHITE = [255, 255, 255] as const;
const WORST_BACKDROP = { light: BLACK, dark: WHITE } as const;

describe('the vibrancy sidebar tint', () => {
  // `text.secondary` is a row's label, `text.metrics` the captions and icons.
  it.each(['light', 'dark'] as const)('keeps the %s sidebar text at WCAG AA over the worst backdrop', (mode) => {
    const tokens = schemes[mode];
    const surface = composite(parseHex(tokens['background.secondary'] ?? ''), SIDEBAR_TINT_OPACITY, WORST_BACKDROP[mode]);
    for (const token of ['text.secondary', 'text.metrics']) {
      expect(contrastRatio(parseHex(tokens[token] ?? ''), surface), `${mode} ${token}`).toBeGreaterThanOrEqual(4.5);
    }
  });

  it('computes the WCAG ratio', () => {
    expect(contrastRatio(BLACK, WHITE)).toBeCloseTo(21, 5);
    expect(contrastRatio([119, 119, 119], [119, 119, 119])).toBe(1);
  });
});
