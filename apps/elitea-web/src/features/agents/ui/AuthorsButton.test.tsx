import { describe, expect, it, vi } from 'vitest';

import { renderWithProviders } from '../__tests__/testUtils';
import { AuthorsButton, authorInitial, dedupeAuthors } from './AuthorsButton';

const alice = { id: 'a1', name: 'Alice', email: 'alice@example.com' };
const bob = { id: 'a2', name: 'Bob', email: 'bob@example.com' };

describe('dedupeAuthors', () => {
  it('deduplicates by name+id, preserving first-seen order', () => {
    expect(dedupeAuthors([{ author: alice }, { author: bob }, { author: alice }])).toStrictEqual([alice, bob]);
  });

  it('skips versions with no author', () => {
    expect(dedupeAuthors([{}, { author: alice }])).toStrictEqual([alice]);
  });
});

describe('authorInitial', () => {
  it('upper-cases the first letter', () => {
    expect(authorInitial('alice')).toBe('A');
  });

  it('falls back to "?" for a blank name', () => {
    expect(authorInitial('   ')).toBe('?');
  });
});

describe('AuthorsButton', () => {
  it('renders one avatar per unique author', () => {
    const { getByText } = renderWithProviders(<AuthorsButton versions={[{ author: alice }, { author: bob }]} />);
    expect(getByText('A')).toBeInTheDocument();
    expect(getByText('B')).toBeInTheDocument();
  });

  it('shows a placeholder avatar, not a crash, for a deleted author (#6702)', () => {
    const gone = { id: 'a9', name: '', email: '' };
    const { getByTestId } = renderWithProviders(<AuthorsButton versions={[{ author: gone }]} />);
    expect(getByTestId('deleted-author-avatar')).toBeInTheDocument();
  });

  it('renders nothing when there is no author anywhere', () => {
    const { container } = renderWithProviders(<AuthorsButton versions={[{}]} />);
    expect(container).toBeEmptyDOMElement();
  });

  it('calls onSelectAuthor with the clicked author', () => {
    const onSelectAuthor = vi.fn();
    const { getByText } = renderWithProviders(
      <AuthorsButton
        versions={[{ author: alice }]}
        onSelectAuthor={onSelectAuthor}
      />,
    );
    getByText('A').click();
    expect(onSelectAuthor).toHaveBeenCalledWith(alice);
  });
});
