/**
 * The composer's attach control, in all three of the forms it renders.
 *
 * It had no test file of its own before this one, which is part of why the
 * chat composer could take a file and show nothing: every assertion about
 * attachments lived either below this component (`useAttachmentState`,
 * `validateAttachmentFiles`) or above it (`PlusChatButton`'s drop handle), and
 * the control itself — the counter, the disabled ceiling, the rejection
 * message, and the shape it takes when it is only a drop target — was covered
 * by neither.
 */
import { createRef } from 'react';

import { ThemeProvider } from '@mui/material/styles';
import { fireEvent, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { ATTACHMENT_LIMITS } from '@/shared/lib/attachments';

import { AttachmentButton } from './AttachmentButton';
import type { AttachmentButtonHandle, AttachmentButtonProps } from './AttachmentButton';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

function renderButton(props: AttachmentButtonProps & { readonly handleRef?: ReturnType<typeof createRef<AttachmentButtonHandle>> } = {}) {
  const { handleRef, ...rest } = props;
  return render(
    <ThemeProvider
      theme={theme}
      defaultMode={DEFAULT_COLOR_SCHEME}
    >
      <AttachmentButton
        ref={handleRef}
        {...rest}
      />
    </ThemeProvider>,
  );
}

function picker(): HTMLInputElement {
  const input = document.querySelector<HTMLInputElement>('input[type="file"]');
  if (input === null) throw new Error('Expected the hidden file input');
  return input;
}

/** `input.files` is read-only in jsdom, so the picked set is installed directly. */
function pick(files: readonly File[]): void {
  const input = picker();
  Object.defineProperty(input, 'files', { configurable: true, value: files });
  fireEvent.change(input);
}

describe('AttachmentButton — the icon form', () => {
  it('opens the picker and reports the accepted files', async () => {
    const user = userEvent.setup();
    const onAttachFiles = vi.fn();
    renderButton({ onAttachFiles });

    const opened = vi.spyOn(picker(), 'click');
    await user.click(screen.getByRole('button', { name: 'attach files' }));
    expect(opened).toHaveBeenCalledTimes(1);

    const file = new File(['x'], 'notes.txt', { type: 'text/plain' });
    pick([file]);
    expect(onAttachFiles).toHaveBeenCalledWith([file]);
    // Reset, so re-picking the SAME file fires `change` again.
    expect(picker().value).toBe('');
  });

  it('closes at the file ceiling — button, input and picker all', () => {
    const onAttachFiles = vi.fn();
    const full = Array.from(
      { length: ATTACHMENT_LIMITS.MAX_ATTACHMENTS },
      (_unused, index) => new File(['x'], `f${index}.txt`),
    );
    renderButton({ attachments: full, onAttachFiles });

    // Both halves matter: a disabled BUTTON with a live input still takes a
    // drop, and a live button with a disabled input opens a picker that
    // cannot answer.
    expect(screen.getByRole('button', { name: 'attach files' })).toBeDisabled();
    expect(picker()).toBeDisabled();

    fireEvent.click(screen.getByRole('button', { name: 'attach files' }));
    expect(onAttachFiles).not.toHaveBeenCalled();
  });

  it('drops the files the validator rejects and reports them', () => {
    const onAttachFiles = vi.fn();
    const onError = vi.fn();
    renderButton({ onAttachFiles, onError });

    const tooBig = new File([], 'huge.png', { type: 'image/png' });
    Object.defineProperty(tooBig, 'size', { value: ATTACHMENT_LIMITS.MAX_IMAGE_FILE_SIZE + 1 });
    pick([tooBig]);

    expect(onAttachFiles).not.toHaveBeenCalled();
    expect(onError).toHaveBeenCalledTimes(1);
    expect(String(onError.mock.calls[0]?.[0])).toContain('huge.png');
  });

  it('ignores an empty pick', () => {
    const onAttachFiles = vi.fn();
    const onError = vi.fn();
    renderButton({ onAttachFiles, onError });

    pick([]);

    expect(onAttachFiles).not.toHaveBeenCalled();
    expect(onError).not.toHaveBeenCalled();
  });
});

describe('AttachmentButton — the menu-row form', () => {
  it('shows the remaining capacity as text and activates from the keyboard', async () => {
    const user = userEvent.setup();
    const onAttachFiles = vi.fn();
    renderButton({ showLabel: true, attachments: [new File(['x'], 'one.txt')], onAttachFiles });

    const row = screen.getByTestId('plus-menu-attachments');
    expect(row).toHaveTextContent(`${ATTACHMENT_LIMITS.MAX_ATTACHMENTS - 1} left`);

    const opened = vi.spyOn(picker(), 'click');
    row.focus();
    await user.keyboard('{Enter}');
    expect(opened).toHaveBeenCalledTimes(1);
  });

  it('goes aria-disabled at the ceiling and opens nothing', () => {
    const onError = vi.fn();
    const full = Array.from(
      { length: ATTACHMENT_LIMITS.MAX_ATTACHMENTS },
      (_unused, index) => new File(['x'], `f${index}.txt`),
    );
    renderButton({ showLabel: true, attachments: full, onError });

    const row = screen.getByTestId('plus-menu-attachments');
    expect(row).toHaveAttribute('aria-disabled', 'true');
    expect(row).toHaveTextContent('0 left');
    // A `menuitem` is a div, so `aria-disabled` alone stops no click: the
    // component withholds the handler as well, and it is out of the tab order.
    expect(row).toHaveAttribute('tabindex', '-1');

    const opened = vi.spyOn(picker(), 'click');
    fireEvent.click(row);
    fireEvent.keyDown(row, { key: 'Enter' });
    expect(opened).not.toHaveBeenCalled();
    expect(onError).not.toHaveBeenCalled();
  });
});

describe('AttachmentButton — the drop-target-only form', () => {
  it('renders no control and no input, and still serves the handle', () => {
    const handleRef = createRef<AttachmentButtonHandle>();
    const onAttachFiles = vi.fn();
    const { container } = renderButton({ dropTargetOnly: true, handleRef, onAttachFiles });

    expect(container).toBeEmptyDOMElement();
    expect(screen.queryByRole('button', { name: 'attach files' })).toBeNull();

    const file = new File(['x'], 'dropped.txt');
    handleRef.current?.onDrop({ dataTransfer: { files: [file] }, preventDefault: () => {} });
    expect(onAttachFiles).toHaveBeenCalledWith([file]);
  });

  it('ignores a drop while attachments are disabled', () => {
    const handleRef = createRef<AttachmentButtonHandle>();
    const onAttachFiles = vi.fn();
    renderButton({ dropTargetOnly: true, handleRef, onAttachFiles, disableAttachments: true });

    handleRef.current?.onDrop({ dataTransfer: { files: [new File(['x'], 'dropped.txt')] }, preventDefault: () => {} });
    expect(onAttachFiles).not.toHaveBeenCalled();
  });
});
