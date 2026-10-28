import { describe, expect, it } from 'vitest';

import type { Theme } from '@mui/material/styles';

import { DEFAULT_BRAND_PACK, buildEliteaTheme } from '@/shared/brand';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { cardSx, iconWellSx, ResourceCard, titleSx } from './ResourceCard';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

/** Resolves a (possibly theme-callback) sx field value against the real theme, the way MUI's `sx` prop would. */
function resolve(value: unknown): unknown {
  return typeof value === 'function' ? (value as (t: Theme) => unknown)(theme) : value;
}

describe('cardSx (brand guide v3)', () => {
  it('sits on the elevated surface with a hairline border and the large radius', () => {
    expect(resolve(cardSx.backgroundColor)).toBe(theme.vars.palette.background.modal.simple);
    expect(resolve(cardSx.borderColor)).toBe(theme.vars.palette.border.cardsOutlines);
    expect(resolve(cardSx.borderRadius)).toBe(theme.vars.shape.radiusLg);
  });

  it('has no gradient wash and no border-gradient pseudo-element', () => {
    expect(cardSx).not.toHaveProperty('background');
    expect(cardSx).not.toHaveProperty('&::before');
    expect(JSON.stringify(cardSx)).not.toContain('gradient');
  });

  it('makes the border stronger on hover', () => {
    expect(resolve(cardSx['&:hover'].borderColor)).toBe(theme.vars.palette.border.hover);
  });
});

describe('iconWellSx', () => {
  it('is neutral: surface fill and the muted icon colour, with no per-card token', () => {
    expect(resolve(iconWellSx.backgroundColor)).toBe(theme.vars.palette.background.card.default);
    expect(resolve(iconWellSx.color)).toBe(theme.vars.palette.icon.fill.default);
    expect(JSON.stringify(iconWellSx)).not.toContain('resourceCard');
  });
});

describe('titleSx', () => {
  it('uses the strong text colour and is not tracked uppercase', () => {
    expect(resolve(titleSx.color)).toBe(theme.vars.palette.text.secondary);
    expect(titleSx.textTransform).toBe('none');
    expect(titleSx.fontWeight).toBe(700);
  });
});

const TITLE = 'Documentation';
const DESCRIPTION = 'API reference, guides, and platform concepts';
const EMPTY = 'No links yet.';

describe('ResourceCard', () => {
  it('renders title as a heading, description, and children', () => {
    const { getByText, getByRole } = renderWithTheme(
      <ResourceCard
        title={TITLE}
        description={DESCRIPTION}
        tourTargetId="doc-card"
        icon={<span>icon</span>}
      >
        <span>{EMPTY}</span>
      </ResourceCard>,
    );

    expect(getByRole('heading', { name: TITLE })).toBeInTheDocument();
    expect(getByText(DESCRIPTION)).toBeInTheDocument();
    expect(getByText(EMPTY)).toBeInTheDocument();
  });
});
