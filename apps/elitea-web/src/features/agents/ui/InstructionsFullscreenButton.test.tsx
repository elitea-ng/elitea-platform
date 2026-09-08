import { describe, expect, it, vi } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { installCodeMirrorTestPolyfills } from '@/shared/ui/lib/field/codeMirrorTestPolyfills';

import { renderWithProviders } from '../__tests__/testUtils';
import { InstructionsFullscreenButton } from './InstructionsFullscreenButton';

installCodeMirrorTestPolyfills();

/** CM6's `.cm-content` is the element the dialog's editor actually renders the text into. */
function fullscreenText(): string {
  const dialog = screen.getByTestId('agent-instructions-fullscreen-dialog');
  const content = dialog.querySelector('.cm-content');
  return content?.textContent ?? '';
}

describe('InstructionsFullscreenButton', () => {
  it('opens the instructions on a fullscreen surface, carrying the current text', async () => {
    const user = userEvent.setup();
    renderWithProviders(
      <InstructionsFullscreenButton
        value="You are a helpful assistant."
        onChange={vi.fn()}
      />,
    );

    expect(screen.queryByTestId('agent-instructions-fullscreen-dialog')).not.toBeInTheDocument();
    await user.click(screen.getByTestId('agent-instructions-fullscreen-button'));

    await waitFor(() => expect(screen.getByTestId('agent-instructions-fullscreen-dialog')).toBeInTheDocument());
    // The SAME text, not an empty editor: a dialog that opened blank would
    // read as "your instructions are gone".
    expect(fullscreenText()).toContain('You are a helpful assistant.');
  });

  it('closes again without touching the value', async () => {
    const onChange = vi.fn();
    const user = userEvent.setup();
    renderWithProviders(
      <InstructionsFullscreenButton
        value="Original text."
        onChange={onChange}
      />,
    );

    await user.click(screen.getByTestId('agent-instructions-fullscreen-button'));
    await waitFor(() => expect(screen.getByTestId('agent-instructions-fullscreen-dialog')).toBeInTheDocument());
    await user.keyboard('{Escape}');

    await waitFor(() => expect(screen.queryByTestId('agent-instructions-fullscreen-dialog')).not.toBeInTheDocument());
    // Opening and closing is not an edit. The dialog holds no draft of its
    // own, so there is nothing to commit on close.
    expect(onChange).not.toHaveBeenCalled();
  });

  it('names the control for a screen reader', () => {
    renderWithProviders(
      <InstructionsFullscreenButton
        value=""
        onChange={vi.fn()}
      />,
    );

    expect(screen.getByRole('button', { name: 'Open in full screen' })).toBeInTheDocument();
  });
});
