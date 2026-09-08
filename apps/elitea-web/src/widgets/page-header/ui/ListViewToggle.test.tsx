import { beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { installWebStorageShim } from '@/test/webstorage';

installWebStorageShim();

import { renderHeaderRoute } from '../__tests__/testRouter';
import { readPersistedListView } from '../lib/listViewPersistence';
import { ListViewToggle } from './ListViewToggle';

beforeEach(() => {
  window.localStorage.clear();
});

function toggle() {
  return (
    <ListViewToggle
      pageKey="agents"
      testIdPrefix="agent"
    />
  );
}

describe('ListViewToggle', () => {
  it('starts on cards when neither the URL nor the store says otherwise', async () => {
    renderHeaderRoute(toggle());

    expect(await screen.findByTestId('agent-card-view-button')).toHaveAttribute('aria-pressed', 'true');
    expect(screen.getByTestId('agent-table-view-button')).toHaveAttribute('aria-pressed', 'false');
  });

  it('follows the `view` param the URL already carries', async () => {
    renderHeaderRoute(toggle(), '/agents/all?view=table');

    expect(await screen.findByTestId('agent-table-view-button')).toHaveAttribute('aria-pressed', 'true');
  });

  it('writes the chosen view into the URL, which is the key the list itself reads', async () => {
    const user = userEvent.setup();
    const { router } = renderHeaderRoute(toggle());

    await user.click(await screen.findByTestId('agent-table-view-button'));

    await waitFor(() => expect((router.state.location.search as { view?: string }).view).toBe('table'));
    expect(screen.getByTestId('agent-table-view-button')).toHaveAttribute('aria-pressed', 'true');
  });

  it('remembers the choice per page', async () => {
    const user = userEvent.setup();
    renderHeaderRoute(toggle());

    await user.click(await screen.findByTestId('agent-table-view-button'));

    await waitFor(() => expect(readPersistedListView('agents')).toBe('table'));
    expect(readPersistedListView('pipelines')).toBeUndefined();
  });

  it('restores the remembered choice into the URL on a fresh visit', async () => {
    window.localStorage.setItem('el.list.view.agents', 'table');
    const { router } = renderHeaderRoute(toggle());

    await waitFor(() => expect((router.state.location.search as { view?: string }).view).toBe('table'));
    expect(screen.getByTestId('agent-table-view-button')).toHaveAttribute('aria-pressed', 'true');
  });

  it('leaves an explicit URL choice alone even when the store disagrees', async () => {
    window.localStorage.setItem('el.list.view.agents', 'table');
    const { router } = renderHeaderRoute(toggle(), '/agents/all?view=cards');

    await waitFor(async () => expect(await screen.findByTestId('agent-card-view-button')).toHaveAttribute('aria-pressed', 'true'));
    expect((router.state.location.search as { view?: string }).view).toBe('cards');
  });

  it('ignores a stored value that is not one of the two views', () => {
    window.localStorage.setItem('el.list.view.agents', 'grid');

    expect(readPersistedListView('agents')).toBeUndefined();
  });
});
