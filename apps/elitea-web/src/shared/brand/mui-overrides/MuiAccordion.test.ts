import { describe, expect, it } from 'vitest';

import { buildEliteaTheme } from '../buildTheme';
import { muiOverrides } from '.';
import { DEFAULT_BRAND_PACK } from '../tokens';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

function computedRoot(): Record<string, unknown> {
  const root = muiOverrides().MuiAccordion?.styleOverrides?.root;
  expect(typeof root).toBe('function');
  return (root as (arg: { theme: typeof theme }) => Record<string, unknown>)({ theme });
}

describe('MuiAccordion styleOverrides', () => {
  it('rounds the first-of-type element\'s top corners to radiusSm, via a CSS var (never a raw literal)', () => {
    const style = computedRoot();

    expect(style['&:first-of-type']).toEqual({
      borderTopLeftRadius: theme.vars.shape.radiusSm,
      borderTopRightRadius: theme.vars.shape.radiusSm,
    });
    expect(String(theme.vars.shape.radiusSm)).toContain('var(--el-shape-radiusSm');
  });

  it('rounds the last-of-type element\'s bottom corners to radiusSm', () => {
    const style = computedRoot();

    expect(style['&:last-of-type']).toEqual({
      borderBottomLeftRadius: theme.vars.shape.radiusSm,
      borderBottomRightRadius: theme.vars.shape.radiusSm,
    });
  });

  it('sets nothing else — no border, no overflow, no unconditional radius: production has none of them (measured via getComputedStyle against next.elitea.ai)', () => {
    const style = computedRoot();

    expect(Object.keys(style).sort()).toEqual(['&:first-of-type', '&:last-of-type']);
  });

  it('radiusSm (4px) is the actual gap being closed: the ambient theme.shape.borderRadius MUI\'s own un-overridden Accordion reads is radiusMd (8px), not radiusSm', () => {
    expect(theme.vars.shape.radiusSm).not.toBe(theme.vars.shape.radiusMd);
    expect(theme.shape.borderRadius).toBe(DEFAULT_BRAND_PACK.shape.radiusMd);
    expect(DEFAULT_BRAND_PACK.shape.radiusSm).toBe(4);
    expect(DEFAULT_BRAND_PACK.shape.radiusMd).toBe(8);
  });
});
