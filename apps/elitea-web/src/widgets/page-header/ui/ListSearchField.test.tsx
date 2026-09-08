import { describe, expect, it } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { renderHeaderRoute } from '../__tests__/testRouter';
import { ListSearchField } from './ListSearchField';

describe('ListSearchField', () => {
  it('shows the query the URL already carries', async () => {
    renderHeaderRoute(<ListSearchField data-testid="agent-search-input" />, '/agents/all?query=alpha');

    expect(await screen.findByDisplayValue('alpha')).toBeInTheDocument();
  });

  it('writes what the reader types into the route\'s `query` param', async () => {
    const user = userEvent.setup();
    const { router } = renderHeaderRoute(<ListSearchField data-testid="agent-search-input" />);

    await user.type(await screen.findByTestId('agent-search-input'), 'alpha');

    // `SimpleSearchBar` debounces for 300 ms; `waitFor` is what covers it.
    await waitFor(() => expect((router.state.location.search as { query?: string }).query).toBe('alpha'));
  });

  it('clears the param on Escape', async () => {
    const user = userEvent.setup();
    const { router } = renderHeaderRoute(<ListSearchField data-testid="agent-search-input" />, '/agents/all?query=alpha');

    (await screen.findByTestId('agent-search-input')).focus();
    await user.keyboard('{Escape}');

    await waitFor(() => expect((router.state.location.search as { query?: string }).query ?? '').toBe(''));
  });

  it('replaces the history entry instead of stacking one per keystroke', async () => {
    const user = userEvent.setup();
    const { router } = renderHeaderRoute(<ListSearchField data-testid="agent-search-input" />);
    await screen.findByTestId('agent-search-input');
    const historyLength = (): number => (router.history as { length: number }).length;
    const before = historyLength();

    await user.type(screen.getByTestId('agent-search-input'), 'ab');

    await waitFor(() => expect((router.state.location.search as { query?: string }).query).toBe('ab'));
    expect(historyLength()).toBe(before);
  });
});
