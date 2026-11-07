import { describe, expect, it } from 'vitest';

import { memoryEntry } from '../__tests__/fixtures';
import { rowActions } from './MemoryEntryRow';

const NONE = { userId: 9, canManage: false, canDelete: false, canApprove: false };

describe('rowActions', () => {
  it('offers a manager everything on a pending entry', () => {
    expect(rowActions(memoryEntry({ status: 'pending' }), { userId: 9, canManage: true, canDelete: true, canApprove: true })).toEqual([
      'approve',
      'reject',
      'edit',
      'pin',
      'history',
      'delete',
    ]);
  });

  it('lets a person correct and forget an entry about themself, and nothing more', () => {
    expect(rowActions(memoryEntry({ subject_user_id: 9 }), NONE)).toEqual(['edit', 'history', 'delete']);
  });

  it("offers only the history of someone else's entry", () => {
    expect(rowActions(memoryEntry({ subject_user_id: 3 }), NONE)).toEqual(['history']);
    expect(rowActions(memoryEntry({ subject_user_id: null }), { ...NONE, userId: undefined })).toEqual(['history']);
  });

  it('archives an active entry and restores an archived one', () => {
    const manager = { ...NONE, canManage: true };
    expect(rowActions(memoryEntry({ pinned: true }), manager)).toContain('unpin');
    expect(rowActions(memoryEntry(), manager)).toContain('archive');
    expect(rowActions(memoryEntry({ status: 'archived' }), manager)).toContain('restore');
  });
});
