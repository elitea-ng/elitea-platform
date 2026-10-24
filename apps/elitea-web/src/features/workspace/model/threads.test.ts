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

  it('puts a rewound thread in its source\'s place and lists the source under it as the earlier attempt', () => {
    recordThread('w1', { id: 'a', title: 'a', updatedAt: 1 });
    recordThread('w1', { id: 'src', title: 'try', updatedAt: 2 });
    recordThread('w1', { id: 'b', title: 'b', updatedAt: 3 });
    recordThread('w1', { id: 'r1', title: 'try (fork)', updatedAt: 4 }, 'src');
    expect(readThreads('w1').map((t) => [t.id, t.earlierAttemptOf])).toEqual([
      ['b', undefined],
      ['r1', undefined],
      ['src', 'r1'],
      ['a', undefined],
    ]);
    // Rewound again: both earlier attempts move under the newest thread.
    recordThread('w1', { id: 'r2', title: 'try again', updatedAt: 5 }, 'r1');
    expect(readThreads('w1').map((t) => [t.id, t.earlierAttemptOf])).toEqual([
      ['b', undefined],
      ['r2', undefined],
      ['r1', 'r2'],
      ['src', 'r2'],
      ['a', undefined],
    ]);
    // A source this machine does not list: the rewound thread goes first.
    recordThread('w2', { id: 'r', title: 'r', updatedAt: 1 }, 'gone');
    expect(readThreads('w2').map((t) => t.id)).toEqual(['r']);
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
