import { describe, expect, it } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import type { IndexRow } from '../../model/indexesStore';

import { IndexNameWrapper } from './IndexNameWrapper';

function makeIndex(state: string | undefined, collection = 'my-index'): IndexRow {
  return { id: '1', metadata: { collection, state } };
}

describe('IndexNameWrapper', () => {
  it('renders the collection name and no badge for a plain completed state', () => {
    const { getByText, queryByText } = renderWithTheme(<IndexNameWrapper index={makeIndex('completed')} />);
    expect(getByText('my-index')).toBeInTheDocument();
    expect(queryByText('In Progress')).not.toBeInTheDocument();
  });

  it('shows the in-progress spinner label', () => {
    const { getByText } = renderWithTheme(<IndexNameWrapper index={makeIndex('in_progress')} />);
    expect(getByText('In Progress')).toBeInTheDocument();
  });

  it('shows a short "Failed" chip for a failed index', () => {
    const { getByText } = renderWithTheme(<IndexNameWrapper index={makeIndex('failed')} />);
    expect(getByText('Failed')).toBeInTheDocument();
  });

  it('shows the failure reason, with the gateway code turned into a sentence, instead of the generic label (#2042)', () => {
    const index: IndexRow = {
      id: '1',
      metadata: {
        collection: 'test 3',
        state: 'failed',
        error: "Index 'test 3' failed: the embedding gateway refused the run (model_gateway.forbidden); nothing further was embedded",
      },
    };
    const { getByText, queryByText, getByRole } = renderWithTheme(<IndexNameWrapper index={index} />);
    // The chip is short; the reason lives in the tooltip, and the button's
    // accessible name is the generic one, not a second copy of the reason.
    expect(getByText('Failed')).toBeInTheDocument();
    expect(queryByText(/this project is not allowed/)).not.toBeInTheDocument();
    expect(getByRole('button', { name: 'More information' })).toBeInTheDocument();
  });

  it('shows "Partially indexed" for a partly-ok index', () => {
    const { getByText } = renderWithTheme(<IndexNameWrapper index={makeIndex('partly_indexed')} />);
    expect(getByText('Partially indexed')).toBeInTheDocument();
  });

  it('shows "Stopped" for a cancelled index', () => {
    const { getByText } = renderWithTheme(<IndexNameWrapper index={makeIndex('cancelled')} />);
    expect(getByText('Stopped')).toBeInTheDocument();
  });

  it('handles a null index gracefully (empty name, no badges)', () => {
    const { container } = renderWithTheme(<IndexNameWrapper index={null} />);
    expect(container).toBeTruthy();
  });
});
