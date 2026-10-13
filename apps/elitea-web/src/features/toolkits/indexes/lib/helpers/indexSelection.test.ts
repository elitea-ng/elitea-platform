import { describe, expect, it } from 'vitest';

import type { IndexRow } from '../../model/indexesStore';
import { NEW_INDEX_ID } from '../constants/indexDetails.constants';

import { pickIndexSelection } from './indexSelection';

const row = (id: string, collection: string, extra: Record<string, unknown> = {}): IndexRow => ({ id, metadata: { collection, ...extra } });

const newest = row('n', 'newest', { indexed: 1 });
const older = row('o', 'older', { indexed: 3 });

describe('pickIndexSelection', () => {
  it('selects the first finished row when nothing is selected', () => {
    expect(pickIndexSelection(null, [row('x', 'running'), newest, older])).toBe(newest);
  });

  it('selects nothing when nothing is selected and nothing is finished', () => {
    expect(pickIndexSelection(null, [row('x', 'running')])).toBeNull();
  });

  it('keeps the selected row when other rows sort ahead of it', () => {
    expect(pickIndexSelection(older, [newest, older])).toBeNull();
  });

  it('refreshes a re-run selected row from the list once it has a history', () => {
    const current = row('o', 'older', { indexed: 3, history: [{}] });
    const fresh = row('o', 'older', { indexed: 4, history: [{}, {}] });
    expect(pickIndexSelection(current, [newest, fresh])).toBe(fresh);
  });

  it('moves from the create stub to the row the user just created, wherever it sorts', () => {
    const stub = row(NEW_INDEX_ID, 'older', { state: 'in_progress' });
    expect(pickIndexSelection(stub, [newest, older])).toBe(older);
  });

  it('keeps the create stub while its index is not finished or not listed', () => {
    const stub = row(NEW_INDEX_ID, 'mine', { state: 'in_progress' });
    expect(pickIndexSelection(stub, [newest, older])).toBeNull();
    expect(pickIndexSelection(stub, [newest, row('m', 'mine')])).toBeNull();
  });

  it('keeps a nameless create stub', () => {
    expect(pickIndexSelection(row(NEW_INDEX_ID, 'New Index'), [newest])).toBeNull();
  });

  it('falls back to the first finished row when the selected row is gone', () => {
    expect(pickIndexSelection(row('gone', 'gone', { indexed: 1 }), [newest, older])).toBe(newest);
  });
});
