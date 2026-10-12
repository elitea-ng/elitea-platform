import { afterEach, describe, expect, it } from 'vitest';

import { recordThread } from '@/features/workspace';
import type { StoredTurn, Workspace } from '@/shared/desktop/workspaceIpc';
import { createFakeWorkspaceIpc } from '@/shared/desktop/workspaceIpc.fake';

import { findLocalWorkThread } from './localThreadLookup';

const folder = (id: string): Workspace => ({ id, path: `/code/${id}`, name: id, project_id: 42, is_git: true });

function turn(conversationId: string): StoredTurn {
  return {
    turn_id: 't1', conversation_id: conversationId, conversation_uuid: null, prompt: 'p', mentions: [],
    started_at: 1, finished_at: 2, events: [], changes: null, events_truncated: false, state: 'done', live: false,
  };
}

afterEach(() => localStorage.clear());

describe('findLocalWorkThread', () => {
  it('finds the folder whose thread list remembers the conversation', async () => {
    const ipc = createFakeWorkspaceIpc({ workspaces: [folder('a'), folder('b')] });
    recordThread('b', { id: '77', title: 'fix it', updatedAt: 1 });
    expect(await findLocalWorkThread('77', ipc)).toBe('b');
  });

  it('falls back to the host turn history when the thread list forgot it', async () => {
    const ipc = createFakeWorkspaceIpc({ workspaces: [folder('a'), folder('b')] });
    ipc.setHistory('a', '78', [turn('78')]);
    expect(await findLocalWorkThread('78', ipc)).toBe('a');
  });

  it('answers null when no folder here ran it, or without a host', async () => {
    expect(await findLocalWorkThread('79', createFakeWorkspaceIpc({ workspaces: [folder('a')] }))).toBeNull();
    expect(await findLocalWorkThread('79', undefined)).toBeNull();
  });
});
