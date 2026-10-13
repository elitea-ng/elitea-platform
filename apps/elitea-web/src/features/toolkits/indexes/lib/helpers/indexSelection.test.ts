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

  it('adopts the fresh row when an in-progress selection completed (no history yet)', () => {
    const current = row('r', 'run', { state: 'in_progress' });
    const fresh = row('r', 'run', { state: 'completed', indexed: 2 });
    expect(pickIndexSelection(current, [fresh])).toBe(fresh);
    expect(pickIndexSelection(fresh, [fresh])).toBeNull();
  });

  it('moves from a started create stub to the row the user just created, wherever it sorts', () => {
    const stub = row(NEW_INDEX_ID, 'older', { state: 'in_progress', task_id: 't1' });
    expect(pickIndexSelection(stub, [newest, older])).toBe(older);
  });

  it('prefers the created index id over the name when the stub has one', () => {
    const stub = row(NEW_INDEX_ID, 'dup', { task_id: 't1', index_id: 'b' });
    const a = row('a', 'dup', { indexed: 1 });
    const b = row('b', 'dup', { state: 'completed', indexed: 1 });
    expect(pickIndexSelection(stub, [a, b])).toBe(b);
  });

  it('does not let an existing finished row steal a fresh or rejected stub', () => {
    const existing = row('e', 'New Index', { state: 'completed', indexed: 1 });
    expect(pickIndexSelection(row(NEW_INDEX_ID, 'New Index', { state: '' }), [existing])).toBeNull();
    expect(pickIndexSelection(row(NEW_INDEX_ID, 'New Index', { state: 'failed' }), [existing])).toBeNull();
  });

  it('moves a started stub to its created row when the run failed', () => {
    const stub = row(NEW_INDEX_ID, 'mine', { state: 'in_progress', task_id: 't1' });
    const failed = row('m', 'mine', { state: 'failed' });
    expect(pickIndexSelection(stub, [newest, failed])).toBe(failed);
  });

  it('keeps the create stub while its index is not terminal or not listed', () => {
    const stub = row(NEW_INDEX_ID, 'mine', { state: 'in_progress', task_id: 't1' });
    expect(pickIndexSelection(stub, [newest, older])).toBeNull();
    expect(pickIndexSelection(stub, [newest, row('m', 'mine', { state: 'in_progress' })])).toBeNull();
  });

  it('keeps a nameless create stub', () => {
    expect(pickIndexSelection(row(NEW_INDEX_ID, 'New Index'), [newest])).toBeNull();
  });

  it('falls back to the first finished row when the selected row is gone', () => {
    expect(pickIndexSelection(row('gone', 'gone', { indexed: 1 }), [newest, older])).toBe(newest);
  });
});
