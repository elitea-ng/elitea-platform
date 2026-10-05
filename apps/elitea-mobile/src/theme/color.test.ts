import { BLACK, WHITE, contrastRatio, formatColor, parseColor } from '@/theme/color';

describe('parseColor', () => {
  it.each([
    ['#abc', { r: 170, g: 187, b: 204, a: 1 }],
    ['#6ae8fa', { r: 106, g: 232, b: 250, a: 1 }],
    ['#00000080', { r: 0, g: 0, b: 0, a: 0.502 }],
    ['rgb(1, 2, 3)', { r: 1, g: 2, b: 3, a: 1 }],
    ['rgba(196, 40, 221, 1)', { r: 196, g: 40, b: 221, a: 1 }],
    [' rgba(80, 161, 255, 0.2)', { r: 80, g: 161, b: 255, a: 0.2 }],
    ['RGBA(255,255,255,.5)', { r: 255, g: 255, b: 255, a: 0.5 }],
  ])('accepts %p', (text, expected) => {
    expect(parseColor(text)).toEqual(expected);
  });

  it.each([
    'linear-gradient(222deg, #fff 0%, #000 100%)',
    'url(https://evil.example/x.png)',
    'expression(alert(1))',
    'var(--brand)',
    'red',
    'hsl(10, 20%, 30%)',
    '#ab',
    '#abcd',
    '#ggg',
    'rgb(256, 0, 0)',
    'rgba(1, 2, 3, 1.5)',
    'rgb(1, 2, 3, 0.5)',
    'rgba(1, 2, 3)',
    'rgb(1 2 3)',
    '#fff; background: url(x)',
    42,
    null,
  ])('refuses %p', (value) => {
    expect(parseColor(value)).toBeNull();
  });
});

it('formats opaque colours as hex and translucent ones as rgba', () => {
  expect(formatColor({ r: 14, g: 19, b: 29, a: 1 })).toBe('#0e131d');
  expect(formatColor({ r: 255, g: 255, b: 255, a: 0.1 })).toBe('rgba(255, 255, 255, 0.1)');
});

describe('contrastRatio', () => {
  it('is 21:1 for black on white and 1:1 for a colour on itself', () => {
    expect(contrastRatio(BLACK, WHITE)).toBeCloseTo(21, 1);
    expect(contrastRatio(WHITE, WHITE)).toBeCloseTo(1, 5);
  });

  it('composites a translucent foreground over the background first', () => {
    const halfBlack = { ...BLACK, a: 0.5 };
    expect(contrastRatio(halfBlack, WHITE)).toBeLessThan(contrastRatio(BLACK, WHITE));
    expect(contrastRatio(halfBlack, WHITE)).toBeGreaterThan(3);
  });
});
