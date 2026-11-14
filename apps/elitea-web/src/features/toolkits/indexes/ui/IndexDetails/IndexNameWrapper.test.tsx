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

  it('shows "Index processing error" for a failed index', () => {
    const { getByText } = renderWithTheme(<IndexNameWrapper index={makeIndex('failed')} />);
    expect(getByText('Index processing error')).toBeInTheDocument();
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
    const { getByText, queryByText } = renderWithTheme(<IndexNameWrapper index={index} />);
    expect(getByText(/this project is not allowed to use that model/)).toBeInTheDocument();
    expect(queryByText('Index processing error')).not.toBeInTheDocument();
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
