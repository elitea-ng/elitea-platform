import { beforeEach, describe, expect, it } from 'vitest';

import { mergeIndexesOverlay, useIndexesStore, type IndexRow } from './indexesStore';

beforeEach(() => {
  useIndexesStore.setState({ tempIndexes: [], indexPatches: {}, toolkitScheduler: {}, selectedHistoryItem: null });
});

describe('useIndexesStore', () => {
  it('addTempLocalIndex prepends (newest first)', () => {
    useIndexesStore.getState().addTempLocalIndex({ id: 'a', metadata: {} });
    useIndexesStore.getState().addTempLocalIndex({ id: 'b', metadata: {} });
    expect(useIndexesStore.getState().tempIndexes.map((i) => i.id)).toEqual(['b', 'a']);
  });

  it('updateIndexDepMeta only writes the fields actually provided', () => {
    useIndexesStore.getState().updateIndexDepMeta('idx-1', { state: 'in_progress' });
    useIndexesStore.getState().updateIndexDepMeta('idx-1', { task_id: 'task-9' });
    expect(useIndexesStore.getState().indexPatches['idx-1']).toEqual({ state: 'in_progress', task_id: 'task-9' });
  });

  it('setToolkitScheduler replaces the whole map; removeToolkitSchedule deletes one key', () => {
    useIndexesStore.getState().setToolkitScheduler({ 'my-index': { enabled: true }, other: { enabled: false } });
    useIndexesStore.getState().removeToolkitSchedule('my-index');
    expect(useIndexesStore.getState().toolkitScheduler).toEqual({ other: { enabled: false } });
  });

  it('removeToolkitSchedule is a no-op for an unknown key', () => {
    useIndexesStore.getState().setToolkitScheduler({ a: {} });
    useIndexesStore.getState().removeToolkitSchedule('does-not-exist');
    expect(useIndexesStore.getState().toolkitScheduler).toEqual({ a: {} });
  });

  it('selectHistoryItem sets and clears', () => {
    useIndexesStore.getState().selectHistoryItem({ conversation_id: 'c1' });
    expect(useIndexesStore.getState().selectedHistoryItem).toEqual({ conversation_id: 'c1' });
    useIndexesStore.getState().selectHistoryItem(null);
    expect(useIndexesStore.getState().selectedHistoryItem).toBeNull();
  });

  it('reset clears every field', () => {
    useIndexesStore.getState().addTempLocalIndex({ id: 'a', metadata: {} });
    useIndexesStore.getState().selectHistoryItem({ x: 1 });
    useIndexesStore.getState().reset();
    expect(useIndexesStore.getState()).toMatchObject({
      tempIndexes: [],
      indexPatches: {},
      toolkitScheduler: {},
      selectedHistoryItem: null,
    });
  });
});

describe('mergeIndexesOverlay', () => {
  const server: IndexRow[] = [
    { id: '1', metadata: { collection: 'a', state: 'completed' } },
    { id: '2', metadata: { collection: 'b', state: 'completed' } },
  ];

  it('returns server rows unchanged when there is no overlay', () => {
    expect(mergeIndexesOverlay(server, [], {})).toEqual(server);
  });

  it('prepends temp rows before server rows', () => {
    const temp: IndexRow[] = [{ id: 'new_index', metadata: { collection: 'New Index', state: '' } }];
    const result = mergeIndexesOverlay(server, temp, {});
    expect(result.map((r) => r.id)).toEqual(['new_index', '1', '2']);
  });

  /* elitea_issues: #2757 — the "in progress"/indexing status must be scoped per index id, keyed patches, never a single shared/global flag that leaks onto whichever OTHER index card the user happens to click while indexing runs. */
  it('merges a patch onto the matching row metadata without touching other rows', () => {
    const result = mergeIndexesOverlay(server, [], { '1': { state: 'in_progress', task_id: 't1' } });
    expect(result[0]?.metadata).toEqual({ collection: 'a', state: 'in_progress', task_id: 't1' });
    expect(result[1]?.metadata).toEqual({ collection: 'b', state: 'completed' });
  });
});
