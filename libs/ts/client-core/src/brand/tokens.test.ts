import assert from 'node:assert/strict';
import { describe, it } from 'node:test';

import { BRAND_TOKENS } from './index';

/** WCAG 2.2 relative luminance of an opaque `#rrggbb`. */
function luminance(hex: string): number {
  const channels = [1, 3, 5].map((i) => Number.parseInt(hex.slice(i, i + 2), 16) / 255);
  const [r = 0, g = 0, b = 0] = channels.map((c) => (c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4));
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
}

function contrast(a: string, b: string): number {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x);
  return ((hi ?? 0) + 0.05) / ((lo ?? 0) + 0.05);
}

const HEX = /^#[0-9A-F]{6}$/;

describe('brand tokens', () => {
  const schemes = BRAND_TOKENS.schemes;

  it('both schemes carry the same token names', () => {
    assert.deepEqual(Object.keys(schemes.dark).sort(), Object.keys(schemes.light).sort());
  });

  it('primary is Base Indigo in both schemes', () => {
    assert.equal(schemes.dark.primary, BRAND_TOKENS.palette.indigo.value);
    assert.equal(schemes.light.primary, BRAND_TOKENS.palette.indigo.value);
  });

  // Every pair the guide promises, measured. A pair whose colour is not an
  // opaque hex has no defined ratio, so it fails rather than being skipped.
  for (const pair of BRAND_TOKENS.contrast) {
    for (const scheme of pair.schemes) {
      it(`${scheme}: ${pair.fg} on ${pair.bg} >= ${pair.min}:1`, () => {
        const tokens = schemes[scheme] as Record<string, string>;
        const fg = tokens[pair.fg] ?? '';
        const bg = tokens[pair.bg] ?? '';
        assert.match(fg, HEX);
        assert.match(bg, HEX);
        const ratio = contrast(fg, bg);
        assert.ok(ratio >= pair.min, `${fg} on ${bg} is ${ratio.toFixed(2)}:1`);
      });
    }
  }

  it('the banned pairs really fail AA, so the reasons stay true', () => {
    for (const pair of BRAND_TOKENS.bannedPairs) assert.ok(contrast(pair.fg, pair.bg) < 4.5, pair.why);
  });

  it('no scheme token uses a kill-list colour', () => {
    const killed = new Set(BRAND_TOKENS.killList.flatMap((entry) => entry.values.map((v) => v.toUpperCase())));
    for (const scheme of [schemes.dark, schemes.light]) {
      for (const value of Object.values(scheme)) assert.ok(!killed.has(value.toUpperCase()), value);
    }
  });
});
