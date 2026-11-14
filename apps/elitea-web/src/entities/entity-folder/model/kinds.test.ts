import { describe, expect, it } from 'vitest';

import type { EntityFolder } from '@/shared/api/generated/model';

import { entityIdsInFolder, folderOfEntity, neighboursForStep } from './kinds';

function folder(id: string, ids: number[] = []): EntityFolder {
  return { id, kind: 'skill', name: `F${id}`, position: 0, item_ids: ids, created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z' };
}

describe('folderOfEntity', () => {
  it('finds the folder holding the entity, by a numeric or a string id', () => {
    const folders = [folder('1', [4, 5]), folder('2', [9])];
    expect(folderOfEntity(folders, 9)?.id).toBe('2');
    expect(folderOfEntity(folders, '5')?.id).toBe('1');
  });

  it('answers undefined for an entity at the root', () => {
    expect(folderOfEntity([folder('1', [4])], 7)).toBeUndefined();
  });
});

describe('entityIdsInFolder', () => {
  it('is the folder id set, and undefined when no folder is open', () => {
    const folders = [folder('1', [4, 5])];
    expect(entityIdsInFolder(folders, '1')).toEqual(new Set([4, 5]));
    expect(entityIdsInFolder(folders, undefined)).toBeUndefined();
    expect(entityIdsInFolder(folders, '')).toBeUndefined();
  });

  it('is empty for an id that is not in this list, so a folder of another list shows nothing', () => {
    expect(entityIdsInFolder([folder('1', [4])], '99')).toEqual(new Set());
  });
});

describe('neighboursForStep', () => {
  // Display order, top first: A, B, C.
  const folders = [folder('A'), folder('B'), folder('C')];

  it('moves a folder up by dropping it above the one that was above its neighbour', () => {
    expect(neighboursForStep(folders, 'B', 'up')).toEqual({ above: null, below: 'A' });
    expect(neighboursForStep(folders, 'C', 'up')).toEqual({ above: 'A', below: 'B' });
  });

  it('moves a folder down by dropping it below the one after it', () => {
    expect(neighboursForStep(folders, 'A', 'down')).toEqual({ above: 'B', below: 'C' });
    expect(neighboursForStep(folders, 'B', 'down')).toEqual({ above: 'C', below: null });
  });

  it('is undefined at the ends and for an unknown folder', () => {
    expect(neighboursForStep(folders, 'A', 'up')).toBeUndefined();
    expect(neighboursForStep(folders, 'C', 'down')).toBeUndefined();
    expect(neighboursForStep(folders, 'Z', 'up')).toBeUndefined();
  });
});
