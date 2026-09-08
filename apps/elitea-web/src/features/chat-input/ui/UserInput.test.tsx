import { createRef } from 'react';
import { describe, expect, it, vi } from 'vitest';
import { act, fireEvent, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { UserInput } from './UserInput';
import type { UserInputAttachmentListSlotProps, UserInputHandle, UserInputSendControlSlotProps } from './UserInput';

function getTextarea(): HTMLTextAreaElement {
  return screen.getByTestId('chat-message-input') as HTMLTextAreaElement;
}

describe('UserInput', () => {
  it('renders the send control slot with the current question and calls onSend on Enter', async () => {
    const onSend = vi.fn();
    const sendControl = vi.fn((slotProps: UserInputSendControlSlotProps) => (
      <button
        type="button"
        data-testid="send-btn"
        disabled={slotProps.disabledSend}
        onClick={slotProps.onSend}
      >
        send:{slotProps.question}
      </button>
    ));

    renderWithTheme(
      <UserInput
        slots={{ sendControl }}
        callbacks={{ onSend }}
      />,
    );

    const textarea = getTextarea();
    await userEvent.type(textarea, 'hello world');
    expect(screen.getByTestId('send-btn')).toHaveTextContent('send:hello world');

    fireEvent.keyDown(textarea, { key: 'Enter' });
    expect(onSend).toHaveBeenCalledWith('hello world', 'hello world');
    // clearInputAfterSend defaults to true.
    expect(textarea).toHaveValue('');
  });

  it('inserts a newline on Shift+Enter and Ctrl+Enter instead of sending', () => {
    const onSend = vi.fn();
    renderWithTheme(
      <UserInput
        slots={{}}
        callbacks={{ onSend }}
      />,
    );
    const textarea = getTextarea();
    fireEvent.change(textarea, { target: { value: 'line1' } });
    fireEvent.keyDown(textarea, { key: 'Enter', shiftKey: true });
    expect(onSend).not.toHaveBeenCalled();
  });

  it('does not send when disabledSend is set', () => {
    const onSend = vi.fn();
    renderWithTheme(
      <UserInput
        slots={{}}
        callbacks={{ onSend }}
        disabledSend
      />,
    );
    const textarea = getTextarea();
    fireEvent.change(textarea, { target: { value: 'hi' } });
    fireEvent.keyDown(textarea, { key: 'Enter' });
    expect(onSend).not.toHaveBeenCalled();
  });

  it('renders the attachment list slot only once there are attachments, and the highlight overlay slot only once there is content + ranges', () => {
    const attachmentList = vi.fn((_props: UserInputAttachmentListSlotProps) => <div data-testid="attachment-list" />);
    const highlightOverlay = vi.fn(() => <div data-testid="highlight-overlay" />);

    const { rerender } = renderWithTheme(
      <UserInput
        slots={{ attachmentList, highlightOverlay }}
        attachments={{ items: [] }}
        slotProps={{ highlight: { ranges: [{ start: 0, end: 1 }] } }}
      />,
    );
    expect(screen.queryByTestId('attachment-list')).not.toBeInTheDocument();
    expect(screen.queryByTestId('highlight-overlay')).not.toBeInTheDocument();

    rerender(
      <UserInput
        slots={{ attachmentList, highlightOverlay }}
        attachments={{ items: [new File(['x'], 'a.txt')] }}
        slotProps={{ highlight: { ranges: [{ start: 0, end: 1 }] } }}
      />,
    );
    expect(screen.getByTestId('attachment-list')).toBeInTheDocument();
    const lastCall = attachmentList.mock.calls.at(-1)?.[0];
    expect(lastCall?.attachments).toHaveLength(1);
    expect(lastCall?.attachments[0]).toBeInstanceOf(File);

    const textarea = getTextarea();
    fireEvent.change(textarea, { target: { value: 'x' } });
    expect(screen.getByTestId('highlight-overlay')).toBeInTheDocument();
  });

  it('shows the stop button while streaming (and not uploading), and the send control otherwise', () => {
    const onStop = vi.fn();
    const sendControl = vi.fn(() => <div data-testid="send-slot" />);
    const { rerender } = renderWithTheme(
      <UserInput
        slots={{ sendControl }}
        callbacks={{ onStop }}
        isStreaming
      />,
    );
    expect(screen.queryByTestId('send-slot')).not.toBeInTheDocument();
    const stopButton = screen.getByRole('button', { name: 'Stop generating' });
    fireEvent.click(stopButton);
    expect(onStop).toHaveBeenCalledTimes(1);

    rerender(
      <UserInput
        slots={{ sendControl }}
        callbacks={{ onStop }}
        isStreaming={false}
      />,
    );
    expect(screen.getByTestId('send-slot')).toBeInTheDocument();
  });

  it('forwards pasted files (renamed with a random appendix) to onFilePaste and prevents default', () => {
    const onFilePaste = vi.fn();
    renderWithTheme(
      <UserInput
        slots={{}}
        callbacks={{ onFilePaste }}
      />,
    );
    const textarea = getTextarea();
    const file = new File(['data'], 'photo.png', { type: 'image/png' });
    const items = [{ kind: 'file', getAsFile: () => file }];
    fireEvent.paste(textarea, { clipboardData: { items } });

    expect(onFilePaste).toHaveBeenCalledTimes(1);
    const pasted = onFilePaste.mock.calls[0]?.[0] as File;
    expect(pasted.name).toMatch(/^photo_\d{8}_\d{6}_.+KB\.png$/);
  });

  it('exposes the documented imperative handle', () => {
    const onSend = vi.fn();
    const ref = createRef<UserInputHandle>();
    renderWithTheme(
      <UserInput
        ref={ref}
        slots={{}}
        callbacks={{ onSend }}
      />,
    );

    // Each call is wrapped in `act` — these are direct ref calls, not user
    // events, so React's state update needs an explicit flush before the
    // next call reads back a handle bound to the updated `inputContent`.
    act(() => ref.current?.setValue('abc'));
    expect(getTextarea()).toHaveValue('abc');
    expect(ref.current?.getInputContent()).toBe('abc');

    act(() => ref.current?.replaceRange(0, 3, 'xyz'));
    expect(ref.current?.getInputContent()).toBe('xyz');

    act(() => ref.current?.mentionUser('@bob'));
    expect(ref.current?.getInputContent()).toBe('xyz@bob');
    // Calling again with the same string is a no-op (already included).
    act(() => ref.current?.mentionUser('@bob'));
    expect(ref.current?.getInputContent()).toBe('xyz@bob');

    act(() => ref.current?.sendQuestion());
    expect(onSend).toHaveBeenCalledWith('xyz@bob', 'xyz@bob');
    expect(ref.current?.getInputContent()).toBe('');

    act(() => ref.current?.setValue('one two'));
    act(() => ref.current?.removeSymbol('two'));
    expect(ref.current?.getInputContent()).toBe('one');

    // Baseline-parity edge case: symbol not found truncates the last character.
    act(() => ref.current?.removeSymbol('zzz'));
    expect(ref.current?.getInputContent()).toBe('on');

    act(() => ref.current?.reset());
    expect(ref.current?.getInputContent()).toBe('');

    act(() => ref.current?.focus());
    expect(getTextarea()).toHaveFocus();
  });

  it('insertTextAtCursor inserts at the caret position', () => {
    const ref = createRef<UserInputHandle>();
    renderWithTheme(
      <UserInput
        ref={ref}
        slots={{}}
      />,
    );
    const textarea = getTextarea();
    act(() => {
      fireEvent.change(textarea, { target: { value: 'ac' } });
    });
    act(() => textarea.setSelectionRange(1, 1));
    act(() => ref.current?.insertTextAtCursor('b'));
    expect(ref.current?.getInputContent()).toBe('abc');
  });

  /*
   * THE CARET IS BACK BEFORE THE NEXT KEYSTROKE, not one macrotask later.
   *
   * No timers are run here, and that is the assertion. The caret used to be
   * restored from `setTimeout(…, 0)`, which left the composer live with the
   * caret at the END of the value for one macrotask: everything typed in that
   * window landed there and was then stranded behind the caret when the timer
   * finally fired. Typing `one`, Shift+Enter, `two` came out as
   * `one\nwo…t`. A layout effect leaves no such window.
   */
  it('restores the caret without waiting for a timer', () => {
    const ref = createRef<UserInputHandle>();
    renderWithTheme(
      <UserInput
        ref={ref}
        slots={{}}
      />,
    );
    const textarea = getTextarea();
    act(() => {
      fireEvent.change(textarea, { target: { value: 'ac' } });
    });
    act(() => textarea.setSelectionRange(1, 1));
    act(() => ref.current?.insertTextAtCursor('b'));
    expect(textarea.selectionStart).toBe(2);
    expect(textarea.selectionEnd).toBe(2);
  });

  /*
   * The splice reads the TEXTAREA, not the state copy: a keystroke that has
   * not been committed yet is still part of what the person is looking at,
   * and splicing into the stale copy silently deletes it.
   */
  it('splices into what the textarea holds, not into a state copy one render behind', () => {
    const ref = createRef<UserInputHandle>();
    renderWithTheme(
      <UserInput
        ref={ref}
        slots={{}}
      />,
    );
    const textarea = getTextarea();
    act(() => {
      fireEvent.change(textarea, { target: { value: 'ac' } });
    });
    // The DOM moved on without an event React saw — the shape of a keystroke
    // whose commit has not landed.
    textarea.value = 'acd';
    textarea.setSelectionRange(3, 3);
    act(() => ref.current?.insertTextAtCursor('!'));
    expect(ref.current?.getInputContent()).toBe('acd!');
  });

  it('bubbles mention matches via slotProps.mention.onMentionChange', () => {
    const onMentionChange = vi.fn();
    renderWithTheme(
      <UserInput
        slots={{}}
        slotProps={{ mention: { users: [{ name: 'Alice' }], onMentionChange } }}
      />,
    );
    const textarea = getTextarea();
    fireEvent.change(textarea, { target: { value: 'hi @Alice ' } });
    expect(onMentionChange).toHaveBeenCalledWith(
      expect.arrayContaining([expect.objectContaining({ username: 'Alice', isValid: true })]),
    );
  });
});
