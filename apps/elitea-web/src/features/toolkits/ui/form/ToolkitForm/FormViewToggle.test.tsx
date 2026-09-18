import { fireEvent } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { FormViewToggle } from './FormViewToggle';

describe('FormViewToggle', () => {
  // #923/ELITEA-2815: the toggle is icon-only now (`TabButtonItem` hides the
  // visible label whenever an item carries an icon) — the label survives as
  // the button's accessible name/tooltip, so these query by role, not text.
  it('renders Form and Raw Json as icon-only buttons, named by their labels', () => {
    const { getByRole, queryByText } = renderWithTheme(
      <FormViewToggle
        view="form"
        onChangeView={vi.fn()}
      />,
    );
    expect(getByRole('button', { name: 'Form' })).toBeInTheDocument();
    expect(getByRole('button', { name: 'Raw Json' })).toBeInTheDocument();
    expect(queryByText('Form')).not.toBeInTheDocument();
    expect(queryByText('Raw Json')).not.toBeInTheDocument();
  });

  it('calls onChangeView with the newly selected view', () => {
    const onChangeView = vi.fn();
    const { getByRole } = renderWithTheme(
      <FormViewToggle
        view="form"
        onChangeView={onChangeView}
      />,
    );
    fireEvent.click(getByRole('button', { name: 'Raw Json' }));
    expect(onChangeView).toHaveBeenCalledWith('json');
  });

  it('does not call onChangeView when clicking the already-selected view', () => {
    const onChangeView = vi.fn();
    const { getByRole } = renderWithTheme(
      <FormViewToggle
        view="form"
        onChangeView={onChangeView}
      />,
    );
    fireEvent.click(getByRole('button', { name: 'Form' }));
    expect(onChangeView).not.toHaveBeenCalled();
  });

  it('defaults to the Form view when no view is supplied', () => {
    const { getByRole } = renderWithTheme(
      <FormViewToggle onChangeView={vi.fn()} />,
    );
    expect(getByRole('button', { name: /Form/ })).toHaveAttribute('aria-pressed', 'true');
  });
});
