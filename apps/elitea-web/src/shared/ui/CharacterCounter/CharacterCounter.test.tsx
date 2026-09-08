import { describe, expect, it } from 'vitest';

import { renderWithTheme } from '../lib/testTheme';
import { CharacterCounter } from '.';

describe('CharacterCounter', () => {
  it('shows the remaining character count', () => {
    const { getByText } = renderWithTheme(<CharacterCounter value="abc" maxLength={10} />);
    expect(getByText('7 characters left')).toBeInTheDocument();
  });

  it('appends the limit-reached message at zero remaining', () => {
    const { getByText } = renderWithTheme(<CharacterCounter value="abcde" maxLength={5} />);
    expect(getByText(/0 characters left/)).toBeInTheDocument();
    expect(getByText(/MAXIMUM character limit/)).toBeInTheDocument();
  });

  it('does not append the limit-reached message before the limit', () => {
    const { queryByText } = renderWithTheme(<CharacterCounter value="abc" maxLength={10} />);
    expect(queryByText(/MAXIMUM character limit/)).not.toBeInTheDocument();
  });

  it('forwards data-testid', () => {
    const { getByTestId } = renderWithTheme(
      <CharacterCounter value="abc" maxLength={10} data-testid="counter" />,
    );
    expect(getByTestId('counter')).toBeInTheDocument();
  });
});

/*
 * The three properties this component grew when it stopped being dead code:
 * the reserved-but-hidden line (#848), the clamp, and the two colours the
 * legacy suite reads off the element
 * (`agents/test_agent_character_limits.py`).
 */
describe('CharacterCounter — the states its five callers depend on', () => {
  it('turns a different colour at the limit', () => {
    const { getByTestId, rerender } = renderWithTheme(
      <CharacterCounter value="short" maxLength={32} data-testid="counter" />,
    );
    const below = getComputedStyle(getByTestId('counter')).color;

    rerender(<CharacterCounter value={'x'.repeat(32)} maxLength={32} data-testid="counter" />);
    const atLimit = getComputedStyle(getByTestId('counter')).color;

    // The exact colour is the theme's (`text.warningText`, an AA-contrast red
    // — see the component's own doc comment); what this asserts is that the
    // two states differ, which is what the legacy suite reads.
    expect(atLimit).not.toBe('');
    expect(atLimit).not.toBe(below);
  });

  it('keeps the line in flow while hidden, so a control under it does not jump (#848)', () => {
    const { getByTestId } = renderWithTheme(
      <CharacterCounter value="hello" maxLength={768} visible={false} data-testid="counter" />,
    );

    expect(getByTestId('counter')).toBeInTheDocument();
    expect(getComputedStyle(getByTestId('counter')).visibility).toBe('hidden');
  });

  it('is visible by default', () => {
    const { getByTestId } = renderWithTheme(
      <CharacterCounter value="hello" maxLength={768} data-testid="counter" />,
    );

    expect(getComputedStyle(getByTestId('counter')).visibility).toBe('visible');
  });

  it('never reports a negative count for a value stored under an older, larger limit', () => {
    const { getByTestId } = renderWithTheme(
      <CharacterCounter value={'x'.repeat(50)} maxLength={32} data-testid="counter" />,
    );

    expect(getByTestId('counter')).toHaveTextContent(
      '0 characters left. You have reached the MAXIMUM character limit',
    );
  });
});
