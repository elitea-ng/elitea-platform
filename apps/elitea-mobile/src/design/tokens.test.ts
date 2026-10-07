import { parseColor } from '@/theme/color';

import { proofRatio } from './contrast';
import { colors, elevation, NON_TEXT_PROOF_KEYS, PROOFS, radius, size, space, type Scheme } from './tokens';

// The documented source of the values (docs/design-system.md §2).
const spec = require('../../docs/design-tokens.json');

const SCHEMES: Scheme[] = ['light', 'dark'];

describe('tokens match docs/design-tokens.json', () => {
  it.each(SCHEMES)('%s colours', (scheme) => {
    expect(JSON.parse(JSON.stringify(colors[scheme]))).toEqual(spec.color[scheme]);
  });

  it('proof pairs', () => {
    expect(PROOFS.map((p) => [...p])).toEqual(spec.proofs);
  });

  it('spacing, radii and sizes', () => {
    const { aliases, ...steps } = spec.space;
    expect(space).toEqual({ ...steps, ...aliases });
    const { usage, note: _note, ...scale } = spec.radius;
    expect(Object.fromEntries(Object.entries(radius).filter(([k]) => k in scale))).toEqual(scale);
    for (const [role, step] of Object.entries(usage as Record<string, keyof typeof scale>)) {
      expect([role, radius[role as keyof typeof radius]]).toEqual([role, scale[step]]);
    }
    expect(size.hitTarget).toEqual(spec.size.hitTarget);
    expect(size.avatar).toEqual(spec.size.avatar);
    expect(size.maxContentWidth).toBe(spec.size.maxContentWidth);
  });

  it.each(SCHEMES)('%s elevation', (scheme) => {
    for (const level of ['e1', 'e2', 'e3'] as const) {
      const want = spec.elevation[scheme][level];
      expect(elevation[scheme][level]).toEqual({
        shadowColor: want.shadowColor,
        shadowOpacity: want.opacity,
        shadowRadius: want.radius,
        shadowOffset: { width: 0, height: want.offsetY },
        elevation: want.androidElevation,
      });
    }
  });

  it.each(SCHEMES)('every %s colour token is a plain colour the strict parser accepts', (scheme) => {
    for (const [key, value] of Object.entries(colors[scheme])) {
      if (typeof value === 'string') expect([key, parseColor(value)]).not.toEqual([key, null]);
    }
  });
});

describe('WCAG proofs (docs/design-system.md §2.1)', () => {
  const rows = SCHEMES.flatMap((scheme) => PROOFS.map(([fg, bg]) => [scheme, fg, bg] as const));

  it.each(rows)('%s: %s on %s', (scheme, fg, bg) => {
    const bar = NON_TEXT_PROOF_KEYS.includes(fg) ? 3 : 4.5;
    expect(proofRatio(colors[scheme], fg, bg)).toBeGreaterThanOrEqual(bar);
  });

  // The iOS 26 native tab bar and bar-button badges ignore the requested text colour and draw a
  // white numeral (src/ui/tabs.ts, src/ui/header.tsx), so the badge fill must pass with white.
  it.each(SCHEMES)('%s: the badge passes 4.5:1 with the white numeral the native badges draw', (scheme) => {
    expect(colors[scheme].badgeText).toBe('#FFFFFF');
    expect(proofRatio({ ...colors[scheme], badgeText: '#FFFFFF' }, 'badgeText', 'badge')).toBeGreaterThanOrEqual(4.5);
  });

  // Spot checks against the published table, so the proof code itself is checked too.
  it.each([
    ['light', 'textPrimary', 'background', 16.66],
    ['dark', 'textPrimary', 'background', 16.89],
    ['light', 'textTertiary', 'backgroundGradient', 4.79],
    ['dark', 'textTertiary', 'surfaceGlass', 5.28],
    ['light', 'accent', 'surfaceGlass', 5.64],
    ['dark', 'accentOn', 'accent', 7.8],
    ['light', 'separatorStrong', 'background', 3.34],
    ['dark', 'focus', 'backgroundGradient', 9.31],
  ] as const)('%s %s on %s is %p', (scheme, fg, bg, ratio) => {
    expect(proofRatio(colors[scheme], fg, bg)).toBeCloseTo(ratio, 1);
  });
});
