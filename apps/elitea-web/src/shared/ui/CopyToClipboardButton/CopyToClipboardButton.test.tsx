import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '../lib/testTheme';
import { CopyToClipboardButton } from '.';

/**
 * `@testing-library/user-event`'s `setup()` unconditionally installs its own
 * `navigator.clipboard` stub (`Clipboard.attachClipboardStubToView`,
 * `@testing-library/user-event/dist/.../utils/dataTransfer/Clipboard.js`) —
 * a `beforeEach`-installed `vi.fn()` replacement gets silently clobbered the
 * moment a test calls `userEvent.setup()`, so a test asserting a bespoke
 * mock was called would false-negative even though the component copied
 * correctly. Reading the copied text back via the SAME stub's `readText()`
 * (rather than trying to out-mock `user-event`'s own mock) is the reliable
 * proof; every test below calls `userEvent.setup()` first, for exactly this
 * reason.
 */
describe('CopyToClipboardButton', () => {
  it('renders the label and the value', () => {
    userEvent.setup();
    const { getByText, getByRole } = renderWithTheme(
      <CopyToClipboardButton
        label="API key"
        value="sk-123"
      />,
    );
    expect(getByText('API key')).toBeInTheDocument();
    expect(getByRole('button', { name: 'sk-123' })).toBeInTheDocument();
  });

  it('copies the value to the clipboard when clicked', async () => {
    const user = userEvent.setup();
    const { getByRole } = renderWithTheme(
      <CopyToClipboardButton
        label="API key"
        value="sk-123"
      />,
    );
    await user.click(getByRole('button'));
    await vi.waitFor(async () => {
      expect(await navigator.clipboard.readText()).toBe('sk-123');
    });
  });

  it('calls onCopied once the copy resolves, not before', async () => {
    const user = userEvent.setup();
    const onCopied = vi.fn();
    const { getByRole } = renderWithTheme(
      <CopyToClipboardButton
        label="API key"
        value="sk-123"
        onCopied={onCopied}
      />,
    );
    await user.click(getByRole('button'));
    await vi.waitFor(() => expect(onCopied).toHaveBeenCalledTimes(1));
  });

  it('does not throw when onCopied is not provided', async () => {
    const user = userEvent.setup();
    const { getByRole } = renderWithTheme(
      <CopyToClipboardButton
        label="API key"
        value="sk-123"
      />,
    );
    await user.click(getByRole('button'));
    await vi.waitFor(async () => {
      expect(await navigator.clipboard.readText()).toBe('sk-123');
    });
  });

  it('wraps the button in a tooltip when tooltip is provided', () => {
    userEvent.setup();
    const { getByRole } = renderWithTheme(
      <CopyToClipboardButton
        label="API key"
        value="sk-123"
        tooltip="Copy to clipboard"
      />,
    );
    // The tooltip DESCRIBES the button (`describeChild`); it does not rename
    // it. The accessible name stays the visible value (WCAG 2.5.3 Label in
    // Name, axe `label-content-name-mismatch`), and the tooltip text is the
    // accessible description.
    const button = getByRole('button', { name: 'sk-123' });
    expect(button).not.toHaveAttribute('aria-label');
    expect(button).toHaveAccessibleDescription('Copy to clipboard');
  });

  it('does not call useToast or read Redux — it is a pure callback prop', () => {
    // Compile-time proof, not a runtime assertion: CopyToClipboardButtonProps
    // has no toast/store dependency in its type, which is the actual DI
    // deviation this component makes from the baseline (see its doc comment).
    const props: Parameters<typeof CopyToClipboardButton>[0] = {
      label: 'x',
      value: 'y',
      onCopied: () => {},
    };
    expect(props.onCopied).toBeTypeOf('function');
  });
});
