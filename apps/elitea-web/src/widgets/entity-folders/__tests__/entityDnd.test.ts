import { describe, expect, it, vi } from 'vitest';

import type { EntityFolder } from '@/entities/entity-folder';

import { ROOT_DROP_ID, entityAnnouncementLabels, entityDndPolicy, entityDragId, entityIdOfDragId, folderDropId } from '../lib/entityDnd';

const FOLDERS = [
  { id: '3', name: 'Support', item_ids: [11, 12] },
  { id: '1', name: 'Release', item_ids: [] },
] as unknown as EntityFolder[];

const nameOf = (id: number): string => `Agent ${id}`;

describe('entity drag ids', () => {
  it('round-trips an entity id and refuses any other id', () => {
    expect(entityIdOfDragId(entityDragId(42))).toBe(42);
    expect(entityIdOfDragId('folder-drop-3')).toBeUndefined();
    expect(entityIdOfDragId('entity-x')).toBeUndefined();
    expect(entityIdOfDragId(7)).toBeUndefined();
  });
});

describe('entityDndPolicy', () => {
  const move = vi.fn().mockResolvedValue(undefined);
  const policy = (selectedIds: readonly string[] = []) => entityDndPolicy({ folders: FOLDERS, selectedIds, nameOf, move });

  it('carries the dragged entity with its current folder', () => {
    expect(policy().resolveDrag(entityDragId(11))).toEqual({ items: [{ id: 11, name: 'Agent 11' }], source: FOLDERS[0] });
    expect(policy().resolveDrag(entityDragId(99))).toEqual({ items: [{ id: 99, name: 'Agent 99' }], source: null });
    expect(policy().resolveDrag('not-an-entity')).toBeUndefined();
  });

  it('carries the whole selection when the drag starts on a selected row, and only that row otherwise', () => {
    expect(policy(['11', '12', '13']).resolveDrag(entityDragId(12))?.items.map((item) => item.id)).toEqual([11, 12, 13]);
    expect(policy(['11', '12']).resolveDrag(entityDragId(50))?.items.map((item) => item.id)).toEqual([50]);
    expect(policy(['11']).resolveDrag(entityDragId(11))?.items.map((item) => item.id)).toEqual([11]);
  });

  it('resolves a folder zone and the "no folder" zone, and nothing else', () => {
    expect(policy().resolveDrop(folderDropId('1'))).toEqual({ group: FOLDERS[1], label: 'Release' });
    expect(policy().resolveDrop(ROOT_DROP_ID)).toEqual({ group: null, label: 'No folder' });
    expect(policy().resolveDrop(folderDropId('404'))).toBeUndefined();
    expect(policy().resolveDrop('somewhere')).toBeUndefined();
  });

  it('moves an entity to another folder or back to the root, never onto where it already is', () => {
    const rules = policy();
    expect(rules.canMove({ id: 11, name: 'x' }, FOLDERS[0] as EntityFolder)).toBe(false);
    expect(rules.canMove({ id: 11, name: 'x' }, FOLDERS[1] as EntityFolder)).toBe(true);
    expect(rules.canMove({ id: 11, name: 'x' }, null)).toBe(true);
    expect(rules.canMove({ id: 99, name: 'x' }, null)).toBe(false);
    expect(rules.canMove({ id: 99, name: 'x' }, FOLDERS[1] as EntityFolder)).toBe(true);
  });

  it('writes the move with the folder id, or null at the root', async () => {
    const rules = policy();
    await rules.move({ id: 11, name: 'x' }, FOLDERS[1] as EntityFolder);
    await rules.move({ id: 11, name: 'x' }, null);
    expect(move.mock.calls).toEqual([
      [11, '1'],
      [11, null],
    ]);
  });
});

describe('entityAnnouncementLabels', () => {
  const labels = entityAnnouncementLabels(FOLDERS, nameOf);

  it('names the entity and the zone a screen reader is over', () => {
    expect(labels.itemName(entityDragId(11))).toBe('Agent 11');
    expect(labels.itemName('other')).toBeUndefined();
    expect(labels.targetName(folderDropId('3'))).toBe('Support');
    expect(labels.targetName(ROOT_DROP_ID)).toBe('No folder');
    expect(labels.targetName('other')).toBeUndefined();
  });

  it('speaks the keyboard instructions and every step', () => {
    expect(labels.pickedUp('Agent 11')).toContain('Space to drop');
    expect(labels.over('Agent 11', 'Support')).toBe('Agent 11 is over Support.');
    expect(labels.dropped('Agent 11', 'Support')).toBe('Agent 11 dropped on Support.');
    expect(labels.cancelled('Agent 11')).toBe('Moving Agent 11 cancelled.');
    expect(labels.overNothing('Agent 11')).toBe('Agent 11 is not over a folder.');
  });
});
