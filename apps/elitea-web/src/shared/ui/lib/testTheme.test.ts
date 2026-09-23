import { describe, expect, it } from 'vitest';

import { remToPx } from './testTheme';

describe('remToPx', () => {
  it('resolves a rem string against jsdom’s 16px root font size', () => {
    expect(remToPx('13.5rem')).toBe('216px');
    expect(remToPx('0.75rem')).toBe('12px');
  });

  it('accepts a bare number as a rem count', () => {
    expect(remToPx(4)).toBe('64px');
  });

  it('refuses a length in any other unit rather than passing it through', () => {
    // A pass-through would make `toBe(remToPx('216px'))` succeed against a
    // computed `'216px'` while proving nothing about the rem declaration.
    expect(() => remToPx('216px')).toThrow(/expects a rem length/);
    expect(() => remToPx('2em')).toThrow(/expects a rem length/);
    // The theme types a `fontSize` as possibly undefined, so the helper takes
    // `undefined` too — and refuses it rather than resolving it to '0px'.
    expect(() => remToPx(undefined)).toThrow(/expects a rem length/);
  });
});
