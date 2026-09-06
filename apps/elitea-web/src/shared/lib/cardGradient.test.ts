import { describe, expect, it } from 'vitest';

import { DEFAULT_BRAND_PACK, buildEliteaTheme } from '@/shared/brand';

import { cardGradientSx } from './cardGradient';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

describe('cardGradientSx', () => {
  it('includes a hover style block by default', () => {
    const sx = cardGradientSx(theme);
    expect(sx).toHaveProperty('&:hover');
    expect(sx.background).toBe(theme.vars.palette.background.card.gradientDark);
  });

  it('omits the hover style block when enableHover is false', () => {
    const sx = cardGradientSx(theme, { enableHover: false });
    expect(sx).not.toHaveProperty('&:hover');
    // The base gradient/border styling is unaffected by the hover toggle.
    expect(sx.background).toBe(theme.vars.palette.background.card.gradientDark);
    expect(sx['&::before']).toBeDefined();
  });
});
