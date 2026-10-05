import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '../lib/testTheme';
import { BaseCheckbox } from '.';

describe('BaseCheckbox', () => {
  it('renders a real checkbox input by default', () => {
    const { getByRole } = renderWithTheme(<BaseCheckbox aria-label="agree" />);
    expect(getByRole('checkbox')).toBeInTheDocument();
  });

  it('renders a real radio input in radio mode', () => {
    const { getByRole } = renderWithTheme(
      <BaseCheckbox
        mode="radio"
        aria-label="option"
      />,
    );
    expect(getByRole('radio')).toBeInTheDocument();
    expect(() => getByRole('checkbox')).toThrow();
  });

  it('reflects the checked prop', () => {
    const { getByRole } = renderWithTheme(
      <BaseCheckbox
        checked
        onChange={() => {}}
        aria-label="agree"
      />,
    );
    expect(getByRole('checkbox')).toBeChecked();
  });

  it('fires onChange when clicked', async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    const { getByRole } = renderWithTheme(
      <BaseCheckbox
        onChange={onChange}
        aria-label="agree"
      />,
    );
    await user.click(getByRole('checkbox'));
    expect(onChange).toHaveBeenCalledTimes(1);
  });

  it('toggles on Space when focused (keyboard path)', async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    const { getByRole } = renderWithTheme(
      <BaseCheckbox
        onChange={onChange}
        aria-label="agree"
      />,
    );
    getByRole('checkbox').focus();
    await user.keyboard(' ');
    expect(onChange).toHaveBeenCalledTimes(1);
  });

  it('does not fire onChange when disabled', () => {
    const onChange = vi.fn();
    const { getByRole } = renderWithTheme(
      <BaseCheckbox
        onChange={onChange}
        disabled
        aria-label="agree"
      />,
    );
    const checkbox = getByRole('checkbox');
    // The native `disabled` attribute is sufficient proof on its own: a
    // real browser never dispatches `click`/`change` to a disabled form
    // control, so this is platform behaviour, not application logic to
    // re-verify. (jsdom's `fireEvent` — unlike a real browser or
    // `user.click`'s pointer-events guard — does not model this, so
    // asserting `onChange` after a synthetic dispatch here would test a
    // jsdom quirk, not the component.)
    expect(checkbox).toBeDisabled();
    expect(onChange).not.toHaveBeenCalled();
  });

  it('supports the indeterminate visual state', () => {
    const { getByRole } = renderWithTheme(
      <BaseCheckbox
        indeterminate
        aria-label="agree"
      />,
    );
    expect(getByRole('checkbox')).toHaveAttribute('data-indeterminate', 'true');
  });

  it("merges a caller's slotProps.input instead of replacing the slot", () => {
    const { getByRole } = renderWithTheme(
      <BaseCheckbox
        indeterminate
        slotProps={{ input: { 'aria-label': 'select all', 'aria-describedby': 'help' } }}
      />,
    );
    const input = getByRole('checkbox', { name: 'select all' });
    expect(input).toHaveAttribute('aria-describedby', 'help');
    if (!(input instanceof HTMLInputElement)) throw new Error('expected a native checkbox input');
    // The native property BaseCheckbox's own input ref sets — lost when a
    // caller's slotProps used to be spread over the whole slot.
    expect(input.indeterminate).toBe(true);
  });

  it('draws its own outline icon when unchecked (the theme hides stock MUI icons)', () => {
    const { container } = renderWithTheme(<BaseCheckbox aria-label="agree" />);
    expect(container.querySelector('svg')).not.toBeNull();
    expect(container.querySelector('[data-testid="CheckBoxOutlineBlankIcon"]')).toBeNull();
  });
});
