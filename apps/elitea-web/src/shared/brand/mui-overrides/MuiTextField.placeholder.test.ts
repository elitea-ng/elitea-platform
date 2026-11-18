/**
 * The "Generate <Entity> with AI" dialogs (skill, agent, project context)
 * give their description box a placeholder, not a floating label. It once
 * rendered dark cyan on the dark dialog and was nearly invisible (#2099).
 * Every one of them is a `variant="standard"` TextField, so the placeholder
 * colour is decided in this one override: it must be a theme token, and the
 * token must be legible on the dialog surface in both colour schemes.
 */
import { describe, expect, it } from 'vitest';

import { buildEliteaTheme } from '../buildTheme';
import { DEFAULT_BRAND_PACK } from '../tokens';

import { muiOverrides } from '.';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

function standardVariantStyle(): Record<string, Record<string, unknown>> {
  const variant = muiOverrides().MuiTextField?.variants?.find((entry) => typeof entry.props !== 'function' && entry.props.variant === 'standard');
  expect(typeof variant?.style).toBe('function');
  return (variant?.style as (arg: { theme: typeof theme }) => Record<string, Record<string, unknown>>)({ theme });
}

function luminance(hex: string): number {
  const channels = [1, 3, 5].map((start) => {
    const value = Number.parseInt(hex.slice(start, start + 2), 16) / 255;
    return value <= 0.03928 ? value / 12.92 : ((value + 0.055) / 1.055) ** 2.4;
  });
  return 0.2126 * channels[0]! + 0.7152 * channels[1]! + 0.0722 * channels[2]!;
}

function contrast(a: string, b: string): number {
  const [light, dark] = [luminance(a), luminance(b)].sort((x, y) => y - x);
  return (light! + 0.05) / (dark! + 0.05);
}

describe('TextField placeholder colour', () => {
  it('is the participant text token, fully opaque, never a raw colour', () => {
    const rule = standardVariantStyle()['& input::placeholder, & textarea::placeholder'];

    expect(rule?.['color']).toBe(theme.vars.palette.text.participant.default);
    expect(String(rule?.['color'])).toContain('var(--');
    expect(rule?.['opacity']).toBe(1);
  });

  it.each(['light', 'dark'] as const)('reads at 4.5:1 or better on the dialog surface in the %s scheme', (scheme) => {
    const flat = findScheme(DEFAULT_BRAND_PACK, scheme);
    const text = flat['text.participant.default'];
    const surface = flat['background.modal.simple'];
    expect(text).toMatch(/^#[0-9A-Fa-f]{6}$/);
    expect(surface).toMatch(/^#[0-9A-Fa-f]{6}$/);
    expect(contrast(text!, surface!)).toBeGreaterThanOrEqual(4.5);
  });
});

/** The pack's per-scheme flat token map, wherever the schema keeps it. */
function findScheme(pack: unknown, scheme: string): Record<string, string> {
  const seen = new Set<unknown>();
  const walk = (node: unknown): Record<string, string> | undefined => {
    if (node === null || typeof node !== 'object' || seen.has(node)) return undefined;
    seen.add(node);
    const record = node as Record<string, unknown>;
    const candidate = record[scheme];
    if (candidate !== null && typeof candidate === 'object' && 'text.participant.default' in candidate) {
      return candidate as Record<string, string>;
    }
    for (const value of Object.values(record)) {
      const found = walk(value);
      if (found !== undefined) return found;
    }
    return undefined;
  };
  const found = walk(pack);
  if (found === undefined) throw new Error(`no ${scheme} token map in the pack`);
  return found;
}
