/**
 * MessageInput's attach control — issue #625 item 2.
 *
 * The unit this component owns is composition, not the network: it picks a
 * file, shows it, and hands it to `onSend` alongside the typed text. Upload
 * and turn-start are `useChat`'s job (chat.hook.test.tsx covers those).
 */
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import MessageInput from './MessageInput';

function setup(overrides: Partial<{ text: string; disabled: boolean }> = {}) {
  const onTextChange = vi.fn();
  const onSend = vi.fn();
  const utils = render(
    <MessageInput
      placeholder="Ask a question"
      text={overrides.text ?? ''}
      onTextChange={onTextChange}
      onSend={onSend}
      disabled={overrides.disabled}
    />,
  );
  return { ...utils, onTextChange, onSend };
}

describe('MessageInput attach control', () => {
  it('renders an attach button and a hidden file input, with no attachment chip yet', () => {
    setup();
    expect(screen.getByRole('button', { name: 'Attach a file' })).toBeInTheDocument();
    expect(screen.queryByText(/notes\.txt/)).not.toBeInTheDocument();
  });

  it('clicking the attach button opens the hidden file input', async () => {
    const user = userEvent.setup();
    setup();
    const fileInput = document.querySelector('input[type="file"]');
    if (!fileInput) throw new Error('no file input rendered');
    const clickSpy = vi.spyOn(fileInput as HTMLInputElement, 'click');

    await user.click(screen.getByRole('button', { name: 'Attach a file' }));

    expect(clickSpy).toHaveBeenCalledOnce();
  });

  it('shows a chip naming the file once one is picked', async () => {
    const user = userEvent.setup();
    setup();
    const file = new File(['hello'], 'notes.txt', { type: 'text/plain' });

    // The file input has no accessible name of its own (it is `hidden`, not
    // `aria-hidden`, per MessageInput.tsx) — found by its type instead.
    const fileInput = document.querySelector('input[type="file"]');
    if (!fileInput) throw new Error('no file input rendered');
    await user.upload(fileInput as HTMLInputElement, file);

    expect(screen.getByText('notes.txt')).toBeInTheDocument();
  });

  it('sends the attached file alongside the text, then clears both', async () => {
    const user = userEvent.setup();
    const { onSend } = setup({ text: 'What does this mean?' });
    const file = new File(['hello'], 'notes.txt', { type: 'text/plain' });
    const fileInput = document.querySelector('input[type="file"]');
    if (!fileInput) throw new Error('no file input rendered');
    await user.upload(fileInput as HTMLInputElement, file);

    await user.click(screen.getByRole('button', { name: 'Send message' }));

    expect(onSend).toHaveBeenCalledWith('What does this mean?', file);
    // The chip is gone — the component's own post-send state, not a second render.
    expect(screen.queryByText('notes.txt')).not.toBeInTheDocument();
  });

  it('sends no file when the attachment was removed before sending', async () => {
    const user = userEvent.setup();
    const { onSend } = setup({ text: 'Just a question' });
    const file = new File(['hello'], 'notes.txt', { type: 'text/plain' });
    const fileInput = document.querySelector('input[type="file"]');
    if (!fileInput) throw new Error('no file input rendered');
    await user.upload(fileInput as HTMLInputElement, file);

    await user.click(screen.getByRole('button', { name: 'Remove attachment' }));
    expect(screen.queryByText('notes.txt')).not.toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Send message' }));
    expect(onSend).toHaveBeenCalledWith('Just a question', undefined);
  });

  it('keeps Send disabled with an attachment but no text — content stays required', async () => {
    const user = userEvent.setup();
    setup({ text: '' });
    const file = new File(['hello'], 'notes.txt', { type: 'text/plain' });
    const fileInput = document.querySelector('input[type="file"]');
    if (!fileInput) throw new Error('no file input rendered');
    await user.upload(fileInput as HTMLInputElement, file);

    expect(screen.getByRole('button', { name: 'Send message' })).toBeDisabled();
  });
});
