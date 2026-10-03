import { fireEvent } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { DEFAULT_BRAND_PACK, buildEliteaTheme } from '@/shared/brand';

import { remToPx, renderWithTheme } from '../lib/testTheme';
import { AuxiliaryTextButton, ShowMoreButton } from '.';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);
const labelSmall = theme.typography.labelSmall;

describe('ShowMoreButton (#6640)', () => {
  it('is a real button that reads "Show more" when closed and "Show less" when open', () => {
    const { getByRole, rerender } = renderWithTheme(
      <ShowMoreButton
        expanded={false}
        onClick={() => {}}
      />,
    );
    const button = getByRole('button', { name: 'Show more' });
    expect(button).toHaveAttribute('aria-expanded', 'false');

    rerender(
      <ShowMoreButton
        expanded
        onClick={() => {}}
      />,
    );
    expect(getByRole('button', { name: 'Show less' })).toHaveAttribute('aria-expanded', 'true');
  });

  it('uses the caller labels and calls onClick', () => {
    const onClick = vi.fn();
    const { getByRole } = renderWithTheme(
      <ShowMoreButton
        expanded={false}
        onClick={onClick}
        moreLabel="Show all"
        lessLabel="Show fewer"
        controls="region-1"
      />,
    );
    const button = getByRole('button', { name: 'Show all' });
    expect(button).toHaveAttribute('aria-controls', 'region-1');
    fireEvent.click(button);
    expect(onClick).toHaveBeenCalledTimes(1);
  });

  it('has 12px text at weight 500, no padding and no underline', () => {
    const { getByRole } = renderWithTheme(
      <ShowMoreButton
        expanded={false}
        onClick={() => {}}
      />,
    );
    const button = getByRole('button', { name: 'Show more' });
    // labelSmall is the 12px rung of the type scale, at weight 500.
    expect(labelSmall.fontWeight).toBe(500);
    expect(button).toHaveStyle({ fontSize: remToPx(theme.typography.labelSmall.fontSize), fontWeight: '500', textDecoration: 'none' });
    expect(getComputedStyle(button).paddingTop).toBe(getComputedStyle(button).paddingBottom);
    expect(Number.parseFloat(getComputedStyle(button).paddingLeft)).toBe(0);
  });

  it('AuxiliaryTextButton has the same look without aria-expanded', () => {
    const { getByRole } = renderWithTheme(<AuxiliaryTextButton onClick={() => {}}>Show</AuxiliaryTextButton>);
    const button = getByRole('button', { name: 'Show' });
    expect(button).not.toHaveAttribute('aria-expanded');
    expect(button).toHaveStyle({ fontSize: remToPx(theme.typography.labelSmall.fontSize), fontWeight: '500' });
  });
});
