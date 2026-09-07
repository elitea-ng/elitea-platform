/**
 * Choosing a source toolkit to add.
 *
 * THE TWO EMPTY STATES ARE DIFFERENT ADVICE, and telling them apart is the
 * whole reason this dialog has a prop for it. "This project has no repository
 * toolkit" sends the user to create one; "every repository toolkit is already
 * a source" tells them they are done. Showing the first to someone who has
 * added everything sends them to create a toolkit they do not need, and the
 * screen looks correct either way — which is why it is asserted here.
 */
import { describe, expect, it, vi } from 'vitest';
import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { renderWithProviders } from '../__tests__/testUtils';
import { AddSourceDialog } from './AddSourceDialog';

const CANDIDATES = [
  { id: '9110', name: 'e2e-github', type: 'github' },
  { id: '9111', name: 'ado repos', type: 'ado_repos' },
];

describe('AddSourceDialog', () => {
  it('lists the candidates and reports which one was chosen', async () => {
    const onAdd = vi.fn();
    renderWithProviders(
      <AddSourceDialog open candidates={CANDIDATES} allAdded={false} onAdd={onAdd} onClose={vi.fn()} />,
    );

    const rows = await screen.findAllByTestId('inventory-source-candidate');
    expect(rows).toHaveLength(2);
    // The TYPE and the ID are on the row: an id is what the user has to
    // recognise when two toolkits share a name, and the type is what says
    // whether the facade can expand it at all.
    expect(screen.getByText('github · 9110')).toBeVisible();

    await userEvent.click(rows[0] as HTMLElement);
    expect(onAdd).toHaveBeenCalledWith('9110');
  });

  it('says the project has no repository toolkit when none exists', async () => {
    renderWithProviders(
      <AddSourceDialog open candidates={[]} allAdded={false} onAdd={vi.fn()} onClose={vi.fn()} />,
    );
    const empty = await screen.findByTestId('inventory-no-candidates');
    expect(empty).toHaveTextContent(/no repository toolkit to ingest from/i);
  });

  it('says every toolkit is already a source when that is what is true', async () => {
    renderWithProviders(
      <AddSourceDialog open candidates={[]} allAdded onAdd={vi.fn()} onClose={vi.fn()} />,
    );
    const empty = await screen.findByTestId('inventory-no-candidates');
    expect(empty).toHaveTextContent(/already a source/i);
    expect(empty).not.toHaveTextContent(/no repository toolkit/i);
  });

  it('closes without adding anything', async () => {
    const onAdd = vi.fn();
    const onClose = vi.fn();
    renderWithProviders(
      <AddSourceDialog open candidates={CANDIDATES} allAdded={false} onAdd={onAdd} onClose={onClose} />,
    );
    await userEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(onClose).toHaveBeenCalledTimes(1);
    expect(onAdd).not.toHaveBeenCalled();
  });

  it('renders nothing while it is closed', () => {
    renderWithProviders(
      <AddSourceDialog open={false} candidates={CANDIDATES} allAdded={false} onAdd={vi.fn()} onClose={vi.fn()} />,
    );
    expect(screen.queryByTestId('inventory-add-source-dialog')).toBeNull();
  });
});
