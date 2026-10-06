import { FIXTURE_ORIGIN, brandPack } from '@/api/__fixtures__/nativeClient';
import { BrandPackSchema, mapBrandPack, type BrandTheme } from '@/theme/brandPack';
import { contrastRatio, parseColor } from '@/theme/color';

import { toOklch } from './oklch';
import { brandInputFrom, DEFAULT_A11Y, resolveDesign, schemeFor } from './resolve';
import { colors } from './tokens';
import { typography } from './typography';

const theme = (over: Partial<BrandTheme> = {}): BrandTheme => ({
  origin: 'https://acme.example',
  productName: 'Acme',
  productDefault: false,
  logoMark: null,
  logoFull: null,
  light: { accent: '#14B8A6', onAccent: '#000000' },
  dark: {},
  fontFaces: [],
  fontFamily: null,
  ...over,
});

describe('schemeFor', () => {
  it.each([
    ['system', 'dark', 'dark'],
    ['system', 'light', 'light'],
    ['system', null, 'light'],
    ['light', 'dark', 'light'],
    ['dark', 'light', 'dark'],
  ] as const)('%s with system %p → %s', (pref, system, want) => {
    expect(schemeFor(pref, system)).toBe(want);
  });
});

/** A pack through the real mapper, with only the given scheme tokens and hue. */
const mapped = (light: Record<string, string>, dark: Record<string, string>, hue: string): BrandTheme =>
  mapBrandPack(BrandPackSchema.parse(brandPack({ brand: { hue }, schemes: { light, dark } })), FIXTURE_ORIGIN, 'file');
const LIGHT_GROUND = { 'background.default': '#FFFFFF', 'text.secondary': '#111111' };
const DARK_GROUND = { 'background.default': '#111111', 'text.secondary': '#FFFFFF' };
const hueOf = (hex: string) => toOklch(parseColor(hex)!).h;

describe('brand pack → design (the pack mapper feeds the unfiltered accent)', () => {
  it('a lemon accent the legacy mapper drops (1.4:1 on white) is corrected, not lost', () => {
    const theme = mapped({ ...LIGHT_GROUND, 'primary.main': '#FFD400' }, DARK_GROUND, '#3355FF');
    expect(theme.light.accent).toBeUndefined(); // the legacy palette still drops it
    const d = resolveDesign({ scheme: 'light', platform: 'ios', brand: brandInputFrom(theme) });
    expect(d.branded).toBe(true);
    expect(d.colors.accent).not.toBe(colors.light.accent);
    expect(Math.abs(hueOf(d.colors.accent) - hueOf('#FFD400'))).toBeLessThan(0.2);
    expect(contrastRatio(parseColor(d.colors.accent)!, parseColor(colors.light.background)!)).toBeGreaterThanOrEqual(4.5);
  });

  it('a pack with only a light accent derives dark from it, not from brand.hue', () => {
    const theme = mapped({ ...LIGHT_GROUND, 'primary.main': '#14B8A6' }, DARK_GROUND, '#3355FF');
    expect(theme.dark.accent).toBe('#3355ff'); // the legacy palette fills the hue
    const fromLight = resolveDesign({ scheme: 'dark', platform: 'ios', brand: { productDefault: false, lightAccent: '#14B8A6' } });
    const d = resolveDesign({ scheme: 'dark', platform: 'ios', brand: brandInputFrom(theme) });
    expect(d.colors.accent).toBe(fromLight.colors.accent);
  });

  it('brand.hue is used when neither scheme has an accent', () => {
    const theme = mapped(LIGHT_GROUND, DARK_GROUND, '#3355FF');
    const d = resolveDesign({ scheme: 'light', platform: 'ios', brand: brandInputFrom(theme) });
    expect(d.branded).toBe(true);
    expect(Math.abs(hueOf(d.colors.accent) - hueOf('#3355FF'))).toBeLessThan(0.2);
  });

  it('a theme persisted without accentSource falls back to the mapped palette', () => {
    const input = brandInputFrom(theme({ accentSource: undefined }));
    expect(input).toMatchObject({ lightAccent: '#14B8A6', darkAccent: null });
  });
});

describe('resolveDesign', () => {
  it('unbranded: the Zefir tokens as published', () => {
    const d = resolveDesign({ scheme: 'dark', platform: 'ios' });
    expect(d.colors).toEqual(colors.dark);
    expect(d.branded).toBe(false);
    expect(d.glass).toBe(true);
    expect(d.elevation.e1.shadowColor).toBe('#000000');
  });

  it('a brand changes only the accent family; neutrals, status and focus stay Zefir', () => {
    const d = resolveDesign({ scheme: 'light', platform: 'ios', brand: brandInputFrom(theme()) });
    expect(d.branded).toBe(true);
    expect(d.colors.accent).toBe('#08786c'); // teal, corrected incl. the tinted gradient stop
    for (const key of ['background', 'surface', 'textPrimary', 'textTertiary', 'focus', 'danger', 'success', 'chip'] as const) {
      expect([key, d.colors[key]]).toEqual([key, colors.light[key]]);
    }
  });

  it('productDefault keeps Zefir rose', () => {
    const d = resolveDesign({ scheme: 'light', platform: 'ios', brand: brandInputFrom(theme({ productDefault: true })) });
    expect(d.colors.accent).toBe(colors.light.accent);
    expect(d.branded).toBe(false);
  });

  it('Reduce Transparency makes glass opaque', () => {
    const d = resolveDesign({ scheme: 'light', platform: 'ios', a11y: { ...DEFAULT_A11Y, reduceTransparency: true } });
    expect(d.glass).toBe(false);
    expect(d.colors.surfaceGlass).toMatchObject({ rgb: colors.light.surfaceGlass.opaqueFallback, alpha: 1, blur: 0 });
  });

  it('Increase Contrast strengthens secondary text and hairlines, and drops glass', () => {
    const d = resolveDesign({ scheme: 'dark', platform: 'ios', a11y: { ...DEFAULT_A11Y, increaseContrast: true } });
    expect(d.colors.textSecondary).toBe(colors.dark.textPrimary);
    expect(d.colors.textTertiary).toBe(colors.dark.textSecondary);
    expect(d.colors.separator).toBe(colors.dark.separatorStrong);
    expect(d.glass).toBe(false);
  });

  it('switches to the accessibility layout at large font scales', () => {
    expect(resolveDesign({ scheme: 'light', platform: 'ios', a11y: { ...DEFAULT_A11Y, fontScale: 1.3 } }).accessibilityLayout).toBe(false);
    expect(resolveDesign({ scheme: 'light', platform: 'ios', a11y: { ...DEFAULT_A11Y, fontScale: 1.35 } }).accessibilityLayout).toBe(true);
  });

  it('reduce motion is passed through', () => {
    expect(resolveDesign({ scheme: 'light', platform: 'android', a11y: { ...DEFAULT_A11Y, reduceMotion: true } }).reduceMotion).toBe(true);
  });
});

describe('typography', () => {
  it('iOS text styles and the M3 scale', () => {
    expect(typography('ios').body).toMatchObject({ fontSize: 17, lineHeight: 22, fontWeight: '400' });
    expect(typography('android').body).toMatchObject({ fontSize: 16, lineHeight: 24, fontWeight: '400' });
    expect(typography('ios').display.fontFamily).toBe('ui-rounded');
    expect(typography('android').display.fontFamily).toBeUndefined();
    expect(typography('ios').code.fontFamily).toBe('Menlo');
    expect(typography('android').code.fontFamily).toBe('monospace');
  });

  it('a brand font takes display and title roles only, never body or metadata', () => {
    const t = typography('ios', 'brand-Acme');
    for (const role of ['display', 'title1', 'title2', 'title3', 'headline'] as const) expect(t[role].fontFamily).toBe('brand-Acme');
    for (const role of ['body', 'callout', 'subheadline', 'footnote', 'caption', 'label'] as const) {
      expect(t[role].fontFamily).toBeUndefined();
    }
    expect(t.code.fontFamily).toBe('Menlo');
  });

  it('every role scales with the system, with a per-role cap', () => {
    for (const style of Object.values(typography('ios'))) expect(style.maxFontSizeMultiplier).toBeGreaterThanOrEqual(1.5);
  });
});
