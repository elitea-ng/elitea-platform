import { describe, expect, it } from 'vitest';

import { remToPx, renderWithTheme } from '../lib/testTheme';
import { GradientIconWrapper } from '.';

describe('GradientIconWrapper', () => {
  it('renders its children', () => {
    const { getByText } = renderWithTheme(<GradientIconWrapper>AI</GradientIconWrapper>);
    expect(getByText('AI')).toBeInTheDocument();
  });

  it('defaults to a 2.75rem frame', () => {
    const { getByText } = renderWithTheme(<GradientIconWrapper>AI</GradientIconWrapper>);
    // jsdom@30 resolves rem against the root font size before reporting a
    // computed length (jsdom@29 echoed the declaration back), so the computed
    // value is px. `remToPx` keeps the assertion pointed at the declaration.
    expect(getByText('AI')).toHaveStyle({ width: remToPx('2.75rem'), height: remToPx('2.75rem') });
  });

  it('accepts a custom size', () => {
    const { getByText } = renderWithTheme(<GradientIconWrapper size="4rem">AI</GradientIconWrapper>);
    expect(getByText('AI')).toHaveStyle({ width: remToPx('4rem'), height: remToPx('4rem') });
  });
});
