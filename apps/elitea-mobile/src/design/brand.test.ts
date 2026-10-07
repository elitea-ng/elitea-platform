import { BLACK, WHITE, contrastRatio, formatColor, parseColor, type Rgba } from '@/theme/color';

import { brandOverrides, correctAccent, resolveBrand } from './brand';
import { accentGrounds, bubbleChipGround, minContrast, rgb } from './contrast';
import { fromOklch, toOklch } from './oklch';
import { colors, type Scheme } from './tokens';

const hex = (value: string): Rgba => rgb(value);
const fmt = (c: Rgba) => formatColor(c).toUpperCase();

describe('OKLCH', () => {
  it.each(['#B4306A', '#FFD400', '#0B1F44', '#14B8A6', '#FFFFFF', '#000000', '#808080'])('round-trips %s', (value) => {
    expect(fmt(fromOklch(toOklch(hex(value))))).toBe(value);
  });

  it('maps an out-of-gamut colour into sRGB by reducing chroma, keeping lightness', () => {
    const out = fromOklch({ l: 0.7, c: 0.5, h: 2 });
    for (const ch of [out.r, out.g, out.b]) expect(ch).toBeGreaterThanOrEqual(0);
    expect(toOklch(out).l).toBeCloseTo(0.7, 1);
  });
});

/** Inputs and results of docs/design-system.md §3.3 (the design's reference script). */
const CASES: [string, string, Record<Scheme, [string, Rgba]>][] = [
  ['Elitea primary.main light', '#C428DD', { light: ['#B810D1', WHITE], dark: ['#DA44F3', BLACK] }],
  ['Elitea primary.main dark', '#6AE8FA', { light: ['#087784', WHITE], dark: ['#6AE8FA', BLACK] }],
  ['lemon', '#FFD400', { light: ['#816B04', WHITE], dark: ['#FFD400', BLACK] }],
  ['navy', '#0B1F44', { light: ['#0B1F44', WHITE], dark: ['#718BB9', BLACK] }],
  ['teal', '#14B8A6', { light: ['#0B796D', WHITE], dark: ['#14B8A6', BLACK] }],
  ['orange', '#F97316', { light: ['#B14F0C', WHITE], dark: ['#F97316', BLACK] }],
  ['Zefir rose', '#B4306A', { light: ['#B4306A', WHITE], dark: ['#E15A90', BLACK] }],
];

describe('correctAccent', () => {
  const rows = CASES.flatMap(([name, input, want]) =>
    (['light', 'dark'] as const).map((scheme) => [name, input, scheme, ...want[scheme]] as const),
  );

  it.each(rows)('%s %s in %s → %s', (_name, input, scheme, accent, on) => {
    const out = correctAccent(hex(input), scheme);
    expect(out).not.toBeNull();
    expect(fmt(out!.accent)).toBe(accent);
    expect(out!.on).toEqual(on);
    // The guarantees, independent of the exact values:
    expect(minContrast(out!.accent, accentGrounds(colors[scheme]))).toBeGreaterThanOrEqual(4.5);
    expect(contrastRatio(out!.on, out!.accent)).toBeGreaterThanOrEqual(4.5);
  });

  it('keeps a passing colour unchanged', () => {
    expect(fmt(correctAccent(hex('#0B1F44'), 'light')!.accent)).toBe('#0B1F44');
  });

  it("uses the pack's button text when it reaches 4.5:1, else white or black", () => {
    const pale = parseColor('#F2A6C3')!; // 2.6:1 on rose
    const ink = parseColor('#1A0010')!;
    expect(correctAccent(hex('#B4306A'), 'light', WHITE)!.on).toEqual(WHITE);
    expect(correctAccent(hex('#B4306A'), 'light', pale)!.on).toEqual(WHITE);
    expect(correctAccent(hex('#FFD400'), 'dark', ink)!.on).toEqual(ink);
  });

  it('refuses a translucent brand colour', () => {
    expect(correctAccent({ ...hex('#B4306A'), a: 0.5 }, 'light')).toBeNull();
  });
});

describe('brandOverrides', () => {
  const rows = CASES.flatMap(([name, input]) => (['light', 'dark'] as const).map((s) => [name, input, s] as const));

  it.each(rows)('%s (%s, %s): every derived pair passes', (_name, input, scheme) => {
    const out = brandOverrides(hex(input), scheme)!;
    const merged = { ...colors[scheme], ...out };
    const pairs: [string, string][] = [
      [merged.accentOn, merged.accent],
      [merged.accentOn, merged.accentPressed],
      [merged.accentSoftOn, merged.accentSoft],
      [merged.chipSelectedText, merged.chipSelected],
      [merged.bubbleUserText, merged.bubbleUser],
    ];
    // A file chip on my own bubble (AttachmentStrip) veils the bubble.
    pairs.push([merged.bubbleUserText, formatColor(bubbleChipGround(merged))]);
    for (const [fg, bg] of pairs) expect(contrastRatio(hex(fg), hex(bg))).toBeGreaterThanOrEqual(4.5);
    // The (possibly tinted) gradient still carries tertiary text and the accent.
    for (const stop of merged.backgroundGradient.stops) {
      expect(contrastRatio(hex(merged.textTertiary), hex(stop))).toBeGreaterThanOrEqual(4.5);
      expect(contrastRatio(hex(merged.accent), hex(stop))).toBeGreaterThanOrEqual(4.5);
    }
  });

  it.each(rows)('%s (%s, %s): the file chip veil never lowers the bubble text contrast', (_name, input, scheme) => {
    const merged = { ...colors[scheme], ...brandOverrides(hex(input), scheme)! };
    const text = hex(merged.bubbleUserText);
    expect(contrastRatio(text, bubbleChipGround(merged))).toBeGreaterThanOrEqual(contrastRatio(text, hex(merged.bubbleUser)));
  });

  it('touches only brand-overridable tokens', () => {
    const out = brandOverrides(hex('#14B8A6'), 'light')!;
    expect(Object.keys(out).sort()).toEqual(
      [
        'accent', 'accentOn', 'accentPressed', 'accentSoft', 'accentSoftOn', 'backgroundGradient',
        'bubbleUser', 'bubbleUserText', 'chipSelected', 'chipSelectedText', 'orb',
      ].sort(),
    );
    expect(out.backgroundGradient!.stops.slice(1)).toEqual(colors.light.backgroundGradient.stops.slice(1));
    expect(out.orb!.stops.slice(1)).toEqual(colors.light.orb.stops.slice(1));
  });

  it('dark user bubble is a deep brand tone, not the bright accent', () => {
    const out = brandOverrides(hex('#14B8A6'), 'dark')!;
    expect(out.bubbleUser).not.toBe(out.accent);
    expect(toOklch(hex(out.bubbleUser!)).l).toBeCloseTo(0.33, 1);
  });

  it('reproduces the Zefir defaults closely from Zefir rose', () => {
    const out = brandOverrides(hex('#B4306A'), 'light')!;
    expect(out.accent).toBe('#b4306a');
    expect(out.accentOn).toBe('#ffffff');
  });
});

describe('resolveBrand (scheme mapping, §3.4)', () => {
  it('productDefault applies no colour at all', () => {
    expect(resolveBrand({ productDefault: true, lightAccent: '#14B8A6', darkAccent: '#14B8A6' })).toEqual({ light: {}, dark: {} });
  });

  it('no brand: nothing', () => {
    expect(resolveBrand(null)).toEqual({ light: {}, dark: {} });
    expect(resolveBrand({ productDefault: false })).toEqual({ light: {}, dark: {} });
  });

  it('each scheme reads its own accent', () => {
    const out = resolveBrand({ productDefault: false, lightAccent: '#C428DD', darkAccent: '#6AE8FA' });
    expect(out.light.accent).toBe('#b402cd'); // §3.3 table + the tinted stop
    expect(out.dark.accent).toBe('#6ae8fa');
  });

  it('derives the missing scheme from the other one', () => {
    const out = resolveBrand({ productDefault: false, lightAccent: '#0B1F44' });
    expect(out.light.accent).toBe('#0b1f44');
    expect(out.dark.accent).toBe('#718bb9');
  });

  it('falls back to brand.hue', () => {
    expect(resolveBrand({ productDefault: false, hue: '#F97316' }).light.accent).toBe('#ae4d06');
  });

  it('ignores unparseable and translucent inputs', () => {
    expect(resolveBrand({ productDefault: false, lightAccent: 'red', darkAccent: 'rgba(1, 2, 3, 0.5)' })).toEqual({
      light: {},
      dark: {},
    });
  });

  it("does not apply one scheme's label colour to an accent borrowed by the other", () => {
    const ink = '#1A0010';
    const out = resolveBrand({ productDefault: false, lightAccent: '#FFD400', lightOn: ink });
    // Light: corrected lemon is dark, so the ink label fails and white is used.
    expect(out.light.accentOn).toBe('#ffffff');
    expect(out.dark.accentOn).toBe('#000000');
  });
});
