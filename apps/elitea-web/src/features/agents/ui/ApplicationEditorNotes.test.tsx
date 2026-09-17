import { describe, expect, it, vi } from 'vitest';
import { fireEvent, screen, within } from '@testing-library/react';

import { renderWithProviders } from '../__tests__/testUtils';

import { ApplicationEditorNotes } from './ApplicationEditorNotes';

describe('ApplicationEditorNotes', () => {
  it('renders the current notes value', () => {
    renderWithProviders(
      <ApplicationEditorNotes
        notes="Some notes"
        onNotesChange={vi.fn()}
      />,
    );
    expect(screen.getByDisplayValue('Some notes')).toBeInTheDocument();
  });

  it('calls onNotesChange with the new value on input', () => {
    const onNotesChange = vi.fn();
    renderWithProviders(
      <ApplicationEditorNotes
        notes=""
        onNotesChange={onNotesChange}
      />,
    );
    // `getByRole('textbox')` — `StyledInputEnhancer` also renders a second
    // (hidden) full-screen-modal `<textarea>`; only one exposes an
    // accessible `textbox` role via the visible label association.
    fireEvent.change(screen.getByRole('textbox', { name: /Notes/i }), { target: { value: 'New notes' } });
    expect(onNotesChange).toHaveBeenCalledWith('New notes');
  });

  it('titles the full-screen edit modal "Editor Notes", not the generic fallback', () => {
    // `StyledInputEnhancer`'s `label` here is a `ReactNode`
    // (`InfoLabelWithTooltip`), not a string, so its own `typeof label ===
    // 'string' ? label : defaultTitle` fallback can't recover "Notes"/
    // "Editor Notes" from it — the baseline passed an explicit
    // `fieldName="Editor Notes"`, ported here as `fullScreenTitle`. Without
    // it, the modal's title falls back to "Edit content".
    renderWithProviders(
      <ApplicationEditorNotes
        notes="Some notes"
        onNotesChange={vi.fn()}
      />,
    );
    fireEvent.click(screen.getByRole('button', { name: 'Full screen view' }));
    const dialog = screen.getByRole('dialog');
    expect(within(dialog).getByText('Editor Notes')).toBeInTheDocument();
    expect(within(dialog).queryByText('Edit content')).not.toBeInTheDocument();
  });

  /* elitea_issues: #5756 — opening the full-screen view alone must not call onNotesChange (Save must not go dirty from this alone) */
  it('does not call onNotesChange just from opening the full-screen view', () => {
    const onNotesChange = vi.fn();
    renderWithProviders(
      <ApplicationEditorNotes
        notes="Some notes"
        onNotesChange={onNotesChange}
      />,
    );
    fireEvent.click(screen.getByRole('button', { name: 'Full screen view' }));
    expect(screen.getByRole('dialog')).toBeInTheDocument();
    expect(onNotesChange).not.toHaveBeenCalled();
  });

  it('disables the field when disabled is true', () => {
    renderWithProviders(
      <ApplicationEditorNotes
        notes=""
        onNotesChange={vi.fn()}
        disabled
      />,
    );
    expect(screen.getByRole('textbox', { name: /Notes/i })).toBeDisabled();
  });
});
