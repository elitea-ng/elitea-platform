import { describe, expect, it, vi } from 'vitest';

import { createHooksIpc, HOOKS_EVENT_CHANNEL, type HooksProposalEvent } from './hooksIpc';
import { WorkspaceIpcError, type ListenFn } from './workspaceIpc';

const noListen: ListenFn = () => Promise.resolve(() => undefined);

describe('createHooksIpc', () => {
  it('maps every command to its host name and snake_case arguments', async () => {
    const invoke = vi.fn().mockResolvedValue(null);
    const ipc = createHooksIpc(invoke, noListen);
    const hook = { id: 'fmt', event: 'PostToolUse' as const, command: ['cargo', 'fmt'] };

    await ipc.list('w1');
    await ipc.save('user', [hook], 'w1');
    await ipc.setEnabled('workspace', 'fmt', false, 'w1');
    await ipc.answerProposal('w1', 'abc', ['fmt']);

    expect(invoke.mock.calls).toEqual([
      ['hooks_list', { workspace_id: 'w1' }],
      ['hooks_save', { scope: 'user', hooks: [hook], workspace_id: 'w1' }],
      ['hooks_set_enabled', { scope: 'workspace', id: 'fmt', enabled: false, workspace_id: 'w1' }],
      ['hooks_proposal_answer', { workspace_id: 'w1', file_sha256: 'abc', accept: ['fmt'] }],
    ]);
  });

  it('rejects with the host code', async () => {
    const ipc = createHooksIpc(vi.fn().mockRejectedValue({ code: 'invalid_request', message: 'bad hook' }), noListen);
    const error: unknown = await ipc.save('user', [], 'w1').catch((caught: unknown) => caught);
    expect(error).toBeInstanceOf(WorkspaceIpcError);
    expect((error as WorkspaceIpcError).code).toBe('invalid_request');
  });

  it('hands over proposals from hooks://event', async () => {
    let deliver: (payload: unknown) => void = () => undefined;
    const listen: ListenFn = (channel, handler) => {
      expect(channel).toBe(HOOKS_EVENT_CHANNEL);
      deliver = handler;
      return Promise.resolve(() => undefined);
    };
    const seen: HooksProposalEvent[] = [];
    await createHooksIpc(vi.fn(), listen).onProposal((event) => seen.push(event));
    deliver({ kind: 'other' });
    deliver({ kind: 'hooks_proposal', workspace_id: 'w1', file: '.elitea/hooks.json', file_sha256: 'abc', hooks: [] });
    expect(seen.map((event) => event.file)).toEqual(['.elitea/hooks.json']);
  });
});
