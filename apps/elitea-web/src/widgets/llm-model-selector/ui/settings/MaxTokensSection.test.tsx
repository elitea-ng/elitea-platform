/**
 * The clear control of `MaxTokensSection` is an icon-only `IconButton`.
 *
 * `ClearIcon` renders an `<svg>` with no text, so the button had an empty
 * accessible name. It sits inside the max-tokens field adornment. A screen
 * reader announced a bare "button" next to the value. The announcement gave
 * no clue about the action.
 *
 * This test queries the control by its accessible name. It fails if the
 * `aria-label` goes away again.
 */
import { describe, expect, it, vi } from 'vitest';
import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { DEFAULT_MAX_TOKENS } from '@/shared/lib/constants';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { MaxTokensSection } from './MaxTokensSection';

describe('MaxTokensSection', () => {
  it('names the clear control and resets the value to the default', async () => {
    const onChange = vi.fn();
    renderWithTheme(
      <MaxTokensSection
        value={2048}
        onChange={onChange}
        onBlur={vi.fn()}
        onFocus={vi.fn()}
      />,
    );

    await userEvent.click(screen.getByRole('button', { name: 'Clear the max tokens value' }));
    expect(onChange).toHaveBeenCalledWith(DEFAULT_MAX_TOKENS);
  });

  it('renders no clear control while the mode is auto', () => {
    renderWithTheme(
      <MaxTokensSection
        value={DEFAULT_MAX_TOKENS}
        onChange={vi.fn()}
        onBlur={vi.fn()}
        onFocus={vi.fn()}
      />,
    );

    expect(
      screen.queryByRole('button', { name: 'Clear the max tokens value' }),
    ).not.toBeInTheDocument();
  });

  it('puts the value input on the same line as the remaining-tokens input (#1966)', () => {
    renderWithTheme(
      <MaxTokensSection value={5000} onChange={vi.fn()} onBlur={vi.fn()} onFocus={vi.fn()} maxOutputTokens={16000} />,
    );

    const valueColumn = screen.getByLabelText('max_tokens').closest('[class*="MuiBox-root"]') as HTMLElement;
    const remainingColumn = screen.getByLabelText('remaining_tokens').closest('[class*="MuiBox-root"]') as HTMLElement;
    // Both columns open with a caption of the same text, so the fields share one top edge;
    // the value column's caption is a hidden spacer.
    const spacer = screen.getByTestId('max-tokens-input-spacer');
    expect(valueColumn).toContainElement(spacer);
    expect(spacer).toHaveAttribute('aria-hidden', 'true');
    expect(spacer.textContent).toBe('Remaining tokens');
    expect(remainingColumn.querySelector('p, span')?.textContent).toBe('Remaining tokens');
  });

  it('renders no spacer while the mode is auto', () => {
    renderWithTheme(<MaxTokensSection value={DEFAULT_MAX_TOKENS} onChange={vi.fn()} onBlur={vi.fn()} onFocus={vi.fn()} />);
    expect(screen.queryByTestId('max-tokens-input-spacer')).not.toBeInTheDocument();
  });
});
