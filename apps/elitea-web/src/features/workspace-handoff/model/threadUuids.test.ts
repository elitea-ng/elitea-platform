import { beforeEach, describe, expect, it } from 'vitest';

import { clearNamespace, createStorage } from '@/shared/lib/storage';

import { readThreadUuids, rememberThreadUuid } from './threadUuids';

beforeEach(() => clearNamespace());

describe('thread UUIDs', () => {
  it('remembers a thread\'s UUID per folder', () => {
    rememberThreadUuid('w1', '77', 'uuid-77');
    rememberThreadUuid('w1', '78', 'uuid-78');
    rememberThreadUuid('w2', '77', 'other');
    expect(readThreadUuids('w1')).toEqual({ '77': 'uuid-77', '78': 'uuid-78' });
    expect(readThreadUuids('w2')).toEqual({ '77': 'other' });
    expect(readThreadUuids('w3')).toEqual({});
  });

  it('reads corrupt storage as none, and lives under the logout-swept namespace', () => {
    createStorage('local').set('desktop.workspace.w1.threadUuids', '{not json');
    expect(readThreadUuids('w1')).toEqual({});
    rememberThreadUuid('w1', '1', 'u');
    expect(readThreadUuids('w1')).toEqual({ '1': 'u' });
    clearNamespace();
    expect(readThreadUuids('w1')).toEqual({});
  });
});
