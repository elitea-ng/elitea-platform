/**
 * MessageInput's attach control — issue #625 item 2, extended by #877
 * (multi-file, drag-and-drop, paste, per-file status).
 *
 * The unit this component owns is composition, not the network: it picks/
 * drops/pastes files, validates them against the shared limits, shows them,
 * and hands them to `onSend` alongside the typed text. Upload and turn-start
 * are `useChat`'s job (chat.hook.test.tsx covers those).
 */
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import MessageInput from './MessageInput';

function setup(overrides: Partial<{ text: string; disabled: boolean }> = {}) {
  const onTextChange = vi.fn();
  const onSend = vi.fn().mockResolvedValue(undefined);
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

function fileInput(): HTMLInputElement {
  const input = document.querySelector('input[type="file"]');
  if (!input) throw new Error('no file input rendered');
  return input as HTMLInputElement;
}

describe('MessageInput attach control', () => {
  it('renders an attach button and a hidden, multi-file input, with no attachment chip yet', () => {
    setup();
    expect(screen.getByRole('button', { name: 'Attach a file' })).toBeInTheDocument();
    expect(fileInput()).toHaveAttribute('multiple');
    expect(screen.queryByText(/notes\.txt/)).not.toBeInTheDocument();
  });

  it('clicking the attach button opens the hidden file input', async () => {
    const user = userEvent.setup();
    setup();
    const clickSpy = vi.spyOn(fileInput(), 'click');

    await user.click(screen.getByRole('button', { name: 'Attach a file' }));

    expect(clickSpy).toHaveBeenCalledOnce();
  });

  it('shows a chip per file once multiple are picked', async () => {
    const user = userEvent.setup();
    setup();
    const fileA = new File(['hello'], 'notes.txt', { type: 'text/plain' });
    const fileB = new File(['world'], 'diagram.png', { type: 'image/png' });

    await user.upload(fileInput(), [fileA, fileB]);

    expect(screen.getByText('notes.txt')).toBeInTheDocument();
    expect(screen.getByText('diagram.png')).toBeInTheDocument();
  });

  it('sends every attached file alongside the text, then clears both', async () => {
    const user = userEvent.setup();
    const { onSend } = setup({ text: 'What does this mean?' });
    const fileA = new File(['hello'], 'notes.txt', { type: 'text/plain' });
    const fileB = new File(['world'], 'diagram.png', { type: 'image/png' });
    await user.upload(fileInput(), [fileA, fileB]);

    await user.click(screen.getByRole('button', { name: 'Send message' }));

    await waitFor(() => expect(onSend).toHaveBeenCalledTimes(1));
    expect(onSend).toHaveBeenCalledWith('What does this mean?', [fileA, fileB], expect.any(Function));
    await waitFor(() => expect(screen.queryByText('notes.txt')).not.toBeInTheDocument());
    expect(screen.queryByText('diagram.png')).not.toBeInTheDocument();
  });

  it('sends undefined attachments when no file is given (the ordinary case)', async () => {
    const user = userEvent.setup();
    const { onSend } = setup({ text: 'Just a question' });

    await user.click(screen.getByRole('button', { name: 'Send message' }));

    await waitFor(() => expect(onSend).toHaveBeenCalledWith('Just a question', undefined, undefined));
  });

  it('sends only the remaining file when one attachment was removed before sending', async () => {
    const user = userEvent.setup();
    const { onSend } = setup({ text: 'Just a question' });
    const fileA = new File(['hello'], 'notes.txt', { type: 'text/plain' });
    const fileB = new File(['world'], 'diagram.png', { type: 'image/png' });
    await user.upload(fileInput(), [fileA, fileB]);

    await user.click(screen.getByRole('button', { name: 'Remove notes.txt' }));
    expect(screen.queryByText('notes.txt')).not.toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Send message' }));
    await waitFor(() => expect(onSend).toHaveBeenCalledWith('Just a question', [fileB], expect.any(Function)));
  });

  it('keeps Send disabled with an attachment but no text — content stays required', async () => {
    const user = userEvent.setup();
    setup({ text: '' });
    const file = new File(['hello'], 'notes.txt', { type: 'text/plain' });
    await user.upload(fileInput(), file);

    expect(screen.getByRole('button', { name: 'Send message' })).toBeDisabled();
  });

  it('rejects a file beyond the shared attachment count limit with an inline error', async () => {
    const user = userEvent.setup();
    setup();
    const files = Array.from({ length: 11 }, (_, i) => new File(['x'], `file-${i}.txt`, { type: 'text/plain' }));

    await user.upload(fileInput(), files);

    expect(screen.getByText(/10-file limit/)).toBeInTheDocument();
    // Only the first 10 were accepted.
    expect(screen.getByText('file-0.txt')).toBeInTheDocument();
    expect(screen.queryByText('file-10.txt')).not.toBeInTheDocument();
  });

  it('adds a pasted image file as an attachment', async () => {
    setup();
    const textarea = screen.getByPlaceholderText('Ask a question');
    const file = new File(['fake-png-bytes'], 'screenshot.png', { type: 'image/png' });
    const items = [{ kind: 'file', getAsFile: () => file }];

    fireEvent.paste(textarea, { clipboardData: { items } });

    expect(await screen.findByText('screenshot.png')).toBeInTheDocument();
  });

  it('adds dropped files as attachments', () => {
    setup();
    const dropZone = screen.getByPlaceholderText('Ask a question').closest('.elitea-assistant-input-area');
    if (!dropZone) throw new Error('no drop zone rendered');
    const file = new File(['hello'], 'dropped.txt', { type: 'text/plain' });

    fireEvent.drop(dropZone, { dataTransfer: { files: [file] } });

    expect(screen.getByText('dropped.txt')).toBeInTheDocument();
  });

  it('attaching a second batch validates against the files already held, not just the incoming ones', async () => {
    const user = userEvent.setup();
    setup();
    const fileA = new File(['hello'], 'first.txt', { type: 'text/plain' });
    const fileB = new File(['world'], 'second.txt', { type: 'text/plain' });

    await user.upload(fileInput(), fileA);
    expect(screen.getByText('first.txt')).toBeInTheDocument();

    await user.upload(fileInput(), fileB);

    expect(screen.getByText('first.txt')).toBeInTheDocument();
    expect(screen.getByText('second.txt')).toBeInTheDocument();
  });

  it('ignores a file-input change that carries no files (a cancelled picker)', () => {
    setup();
    expect(screen.queryByText(/\.txt/)).not.toBeInTheDocument();

    fireEvent.change(fileInput(), { target: { files: null } });

    expect(screen.queryByText(/\.txt/)).not.toBeInTheDocument();
  });

  it('ignores a paste event with no clipboard items, falling through to normal text paste', () => {
    setup();
    const textarea = screen.getByPlaceholderText('Ask a question');

    fireEvent.paste(textarea, { clipboardData: { items: undefined } });

    expect(screen.queryByRole('button', { name: /^Remove/ })).not.toBeInTheDocument();
  });

  it('does nothing on Enter while the message is blank — content stays required', async () => {
    const user = userEvent.setup();
    const { onSend } = setup({ text: '' });
    const textarea = screen.getByPlaceholderText('Ask a question');

    textarea.focus();
    await user.keyboard('{Enter}');

    expect(onSend).not.toHaveBeenCalled();
  });

  it('shows the drop overlay while dragging over, and hides it once the pointer truly leaves', () => {
    setup();
    const dropZone = screen.getByPlaceholderText('Ask a question').closest('.elitea-assistant-input-area');
    if (!dropZone) throw new Error('no drop zone rendered');

    fireEvent.dragOver(dropZone, {});
    expect(screen.getByText('Drop files here')).toBeInTheDocument();

    // A second dragover while already flagged is a no-op re-set.
    fireEvent.dragOver(dropZone, {});
    expect(screen.getByText('Drop files here')).toBeInTheDocument();

    // relatedTarget still inside the zone — dragleave is ignored. jsdom has
    // no real DragEvent, so `fireEvent.dragLeave` cannot carry a genuine
    // Node through `relatedTarget` (it silently drops the property); a
    // manually-dispatched event with the property defined directly is the
    // only way to exercise the "still inside" branch.
    const insideLeave = new Event('dragleave', { bubbles: true, cancelable: false });
    Object.defineProperty(insideLeave, 'relatedTarget', { value: dropZone });
    fireEvent(dropZone, insideLeave);
    expect(screen.getByText('Drop files here')).toBeInTheDocument();

    // relatedTarget outside the zone — the overlay clears.
    fireEvent.dragLeave(dropZone, {});
    expect(screen.queryByText('Drop files here')).not.toBeInTheDocument();
  });

  it('ignores drag-over while disabled — no overlay, no state flip', () => {
    setup({ disabled: true });
    const dropZone = screen.getByPlaceholderText('Ask a question').closest('.elitea-assistant-input-area');
    if (!dropZone) throw new Error('no drop zone rendered');

    fireEvent.dragOver(dropZone, {});

    expect(screen.queryByText('Drop files here')).not.toBeInTheDocument();
  });

  it('threads per-file status updates from onSend through to the attachment chips', async () => {
    const user = userEvent.setup();
    const fileA = new File(['hello'], 'notes.txt', { type: 'text/plain' });
    const fileB = new File(['world'], 'diagram.png', { type: 'image/png' });
    let reportStatus: ((file: File, status: 'pending' | 'uploading' | 'done' | 'error') => void) | undefined;
    let resolveSend: () => void = () => {};
    const onSend = vi.fn(
      (_text: string, _files?: readonly File[], onFileStatus?: typeof reportStatus) =>
        new Promise<void>((resolve) => {
          reportStatus = onFileStatus;
          resolveSend = resolve;
        }),
    );
    render(
      <MessageInput placeholder="Ask a question" text="What is this?" onTextChange={vi.fn()} onSend={onSend} />,
    );
    await user.upload(fileInput(), [fileA, fileB]);

    await user.click(screen.getByRole('button', { name: 'Send message' }));
    await waitFor(() => expect(reportStatus).toBeDefined());

    // Only fileA's entry updates; fileB's chip is left as-is (the `entry.file
    // === file ? … : entry` map touches both arms).
    reportStatus?.(fileA, 'error');

    await waitFor(() => {
      const chip = screen.getByText('notes.txt').closest('.elitea-assistant-file-chip');
      expect(chip).toHaveClass('elitea-assistant-file-chip--error');
    });
    expect(screen.getByText('diagram.png').closest('.elitea-assistant-file-chip')).not.toHaveClass(
      'elitea-assistant-file-chip--error',
    );

    resolveSend();
    await waitFor(() => expect(screen.queryByText('notes.txt')).not.toBeInTheDocument());
  });
});
