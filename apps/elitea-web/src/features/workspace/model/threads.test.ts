import { beforeEach, describe, expect, it } from 'vitest';

import { clearNamespace, createStorage } from '@/shared/lib/storage';

import { forgetThreads, readLastLocation, readThreads, recordThread, writeLastLocation } from './threads';

beforeEach(() => clearNamespace());

describe('folder threads', () => {
  it('puts the latest thread first, keeps one entry per conversation, and caps the list', () => {
    recordThread('w1', { id: '1', title: 'first', updatedAt: 1 });
    recordThread('w1', { id: '2', title: 'second', updatedAt: 2 });
    recordThread('w1', { id: '1', title: 'first again', updatedAt: 3 });
    expect(readThreads('w1').map((t) => [t.id, t.title])).toEqual([
      ['1', 'first again'],
      ['2', 'second'],
    ]);
    expect(readThreads('w2')).toEqual([]);
    for (let i = 0; i < 60; i += 1) recordThread('w1', { id: `n${String(i)}`, title: 'x', updatedAt: i });
    expect(readThreads('w1')).toHaveLength(50);
    forgetThreads('w1');
    expect(readThreads('w1')).toEqual([]);
  });

  it('reads corrupt storage as no threads, and lives under the logout-swept namespace', () => {
    createStorage('local').set('desktop.workspace.w1.threads', '{not json');
    expect(readThreads('w1')).toEqual([]);
    recordThread('w1', { id: '1', title: '  a long  ', updatedAt: 1 });
    expect(readThreads('w1')[0]?.title).toBe('a long');
    clearNamespace();
    expect(readThreads('w1')).toEqual([]);
  });

  it('remembers where the person last was', () => {
    expect(readLastLocation()).toBeNull();
    writeLastLocation({ workspaceId: 'w1', conversationId: '77' });
    expect(readLastLocation()).toEqual({ workspaceId: 'w1', conversationId: '77' });
    writeLastLocation(null);
    expect(readLastLocation()).toBeNull();
  });
});
