/**
 * FolderNameField.limit.test.tsx — the folder name limit of issue 1968: the
 * inline field does not cut what is typed, Enter does not save a name past 128
 * characters, and the counter says by how much it is over.
 */
import { fireEvent, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { FolderNameField } from './ChatsSectionParts';

describe('FolderNameField — name limit', () => {
  it('saves a name of exactly 128 characters on Enter', () => {
    const onDone = vi.fn();
    renderWithTheme(<FolderNameField onDone={onDone} />);
    const input = screen.getByTestId('shell-chats-new-folder-name');
    fireEvent.change(input, { target: { value: 'a'.repeat(128) } });
    fireEvent.keyDown(input, { key: 'Enter' });
    expect(onDone).toHaveBeenCalledWith('a'.repeat(128));
  });

  it('keeps a 129-character name whole, shows the overflow and refuses Enter', () => {
    const onDone = vi.fn();
    renderWithTheme(<FolderNameField onDone={onDone} />);
    const input = screen.getByTestId('shell-chats-new-folder-name') as HTMLInputElement;
    fireEvent.change(input, { target: { value: 'a'.repeat(129) } });
    expect(input.value).toHaveLength(129);
    expect(input).toHaveAttribute('aria-invalid', 'true');
    expect(screen.getByTestId('shell-chats-new-folder-name-counter')).toHaveTextContent('1 characters over the limit');
    fireEvent.keyDown(input, { key: 'Enter' });
    expect(onDone).not.toHaveBeenCalled();
  });

  it('counts an emoji as one character', () => {
    const onDone = vi.fn();
    renderWithTheme(<FolderNameField onDone={onDone} />);
    const input = screen.getByTestId('shell-chats-new-folder-name');
    fireEvent.change(input, { target: { value: '😀'.repeat(128) } });
    fireEvent.keyDown(input, { key: 'Enter' });
    expect(onDone).toHaveBeenCalledTimes(1);
  });
});
