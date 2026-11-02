import { describe, expect, it } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { renderHeaderRoute } from '../__tests__/testRouter';
import { isSearchShortcut, PageSearch } from './PageSearch';

describe('PageSearch', () => {
  it('shows the query the URL already carries', async () => {
    renderHeaderRoute(<PageSearch data-testid="agent-search-input" />, '/agents/all?query=alpha');

    expect(await screen.findByDisplayValue('alpha')).toBeInTheDocument();
  });

  it('writes what the reader types into the route\'s `query` param', async () => {
    const user = userEvent.setup();
    const { router } = renderHeaderRoute(<PageSearch data-testid="agent-search-input" />);

    await user.type(await screen.findByTestId('agent-search-input'), 'alpha');

    // `SimpleSearchBar` debounces for 300 ms; `waitFor` is what covers it.
    await waitFor(() => expect((router.state.location.search as { query?: string }).query).toBe('alpha'));
  });

  it('clears the param on Escape', async () => {
    const user = userEvent.setup();
    const { router } = renderHeaderRoute(<PageSearch data-testid="agent-search-input" />, '/agents/all?query=alpha');

    (await screen.findByTestId('agent-search-input')).focus();
    await user.keyboard('{Escape}');

    await waitFor(() => expect((router.state.location.search as { query?: string }).query ?? '').toBe(''));
  });

  it('replaces the history entry instead of stacking one per keystroke', async () => {
    const user = userEvent.setup();
    const { router } = renderHeaderRoute(<PageSearch data-testid="agent-search-input" />);
    await screen.findByTestId('agent-search-input');
    const historyLength = (): number => (router.history as { length: number }).length;
    const before = historyLength();

    await user.type(screen.getByTestId('agent-search-input'), 'ab');

    await waitFor(() => expect((router.state.location.search as { query?: string }).query).toBe('ab'));
    expect(historyLength()).toBe(before);
  });

  it('focuses on `/` from outside a text field', async () => {
    const user = userEvent.setup();
    renderHeaderRoute(<PageSearch data-testid="agent-search-input" />);
    const input = await screen.findByTestId('agent-search-input');
    expect(input).not.toHaveFocus();

    await user.keyboard('/');

    expect(input).toHaveFocus();
    expect(screen.getByTestId('page-search')).toBeInTheDocument();
  });
});

describe('isSearchShortcut', () => {
  const key = (init: Partial<KeyboardEvent> & { target?: EventTarget | null }) => ({ key: '', metaKey: false, ctrlKey: false, altKey: false, target: document.body, ...init });

  it('takes `/` only outside a text field, and leaves ⌘F / Ctrl+F to the browser’s find', () => {
    const field = document.createElement('input');
    const area = document.createElement('textarea');
    const editable = document.createElement('div');
    editable.contentEditable = 'true';
    // jsdom does not compute `isContentEditable` from the attribute.
    Object.defineProperty(editable, 'isContentEditable', { value: true });
    expect(isSearchShortcut(key({ key: '/' }))).toBe(true);
    expect(isSearchShortcut(key({ key: '/', target: field }))).toBe(false);
    expect(isSearchShortcut(key({ key: '/', target: area }))).toBe(false);
    expect(isSearchShortcut(key({ key: '/', target: editable }))).toBe(false);
    expect(isSearchShortcut(key({ key: 'f', metaKey: true }))).toBe(false);
    expect(isSearchShortcut(key({ key: 'f', ctrlKey: true }))).toBe(false);
    expect(isSearchShortcut(key({ key: '/', metaKey: true }))).toBe(false);
  });
});
