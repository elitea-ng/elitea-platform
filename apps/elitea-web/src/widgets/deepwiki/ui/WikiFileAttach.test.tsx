import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { WikiFileAttach, type WikiFileAttachment } from './WikiFileAttach';

function textFile(name: string, body: string): File {
  return new File([body], name, { type: 'text/plain' });
}

describe('WikiFileAttach', () => {
  it('reads the picked file and hands it to onChange', async () => {
    const onChange = vi.fn();
    renderWithTheme(<WikiFileAttach attachments={[]} onChange={onChange} disabled={false} />);

    await userEvent.upload(
      screen.getByTestId('wiki-chat-attach-input'),
      textFile('notes.md', 'the important bit'),
    );

    await waitFor(() =>
      expect(onChange).toHaveBeenCalledWith([{ name: 'notes.md', content: 'the important bit' }]),
    );
  });

  it('appends to what is already attached rather than replacing it', async () => {
    const onChange = vi.fn();
    const existing: readonly WikiFileAttachment[] = [{ name: 'first.txt', content: 'a' }];
    renderWithTheme(<WikiFileAttach attachments={existing} onChange={onChange} disabled={false} />);

    await userEvent.upload(screen.getByTestId('wiki-chat-attach-input'), textFile('second.txt', 'b'));

    await waitFor(() =>
      expect(onChange).toHaveBeenCalledWith([
        { name: 'first.txt', content: 'a' },
        { name: 'second.txt', content: 'b' },
      ]),
    );
  });

  it('shows a warning and calls nothing when the file type is not allowed', async () => {
    const onChange = vi.fn();
    renderWithTheme(<WikiFileAttach attachments={[]} onChange={onChange} disabled={false} />);

    await userEvent.upload(screen.getByTestId('wiki-chat-attach-input'), textFile('photo.png', 'binary'));

    expect(await screen.findByTestId('wiki-chat-attach-error')).toHaveTextContent(
      'not a supported text file type',
    );
    expect(onChange).not.toHaveBeenCalled();
  });

  it('renders a chip per attached file, with a badge count on the trigger', () => {
    renderWithTheme(
      <WikiFileAttach
        attachments={[
          { name: 'a.md', content: 'x' },
          { name: 'b.txt', content: 'y' },
        ]}
        onChange={vi.fn()}
        disabled={false}
      />,
    );

    expect(screen.getByText('a.md')).toBeVisible();
    expect(screen.getByText('b.txt')).toBeVisible();
    expect(screen.getByText('2')).toBeVisible();
  });

  it('removes an attachment by its own chip, and only that one', async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    renderWithTheme(
      <WikiFileAttach
        attachments={[
          { name: 'a.md', content: 'x' },
          { name: 'b.txt', content: 'y' },
        ]}
        onChange={onChange}
        disabled={false}
      />,
    );

    const chips = screen.getAllByTestId('wiki-chat-attach-chip');
    const chip = chips.find((candidate) => within(candidate).queryByText('a.md') !== null)!;
    await user.click(within(chip).getByTestId('wiki-chat-attach-chip-remove'));

    expect(onChange).toHaveBeenCalledWith([{ name: 'b.txt', content: 'y' }]);
  });

  it('renders no chip row and no trigger content when nothing is attached', () => {
    renderWithTheme(<WikiFileAttach attachments={[]} onChange={vi.fn()} disabled={false} />);
    expect(screen.queryByTestId('wiki-chat-attach-chips')).toBeNull();
  });

  it('disables the trigger while a turn is running', () => {
    renderWithTheme(<WikiFileAttach attachments={[]} onChange={vi.fn()} disabled />);
    expect(screen.getByTestId('wiki-chat-attach-button')).toBeDisabled();
  });
});
