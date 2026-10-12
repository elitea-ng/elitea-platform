import { describe, expect, it, vi } from 'vitest';

import { createIndexIpc, INDEX_EVENT_CHANNEL, OFF_STATUS, type IndexEvent } from './indexIpc';
import { createFakeIndexIpc } from './indexIpc.fake';
import { WorkspaceIpcError, type ListenFn } from './workspaceIpc';

const noListen: ListenFn = () => Promise.resolve(() => undefined);

describe('createIndexIpc', () => {
  it('maps every command to its host name and snake_case arguments', async () => {
    const invoke = vi.fn().mockResolvedValue(null);
    const ipc = createIndexIpc(invoke, noListen);

    await ipc.status('w1');
    await ipc.open('w1');
    await ipc.enable('w1');
    await ipc.disable('w1');
    await ipc.refresh('w1');
    await ipc.refresh('w1', true);
    await ipc.cancel('w1');
    await ipc.remove('w1');

    expect(invoke.mock.calls).toEqual([
      ['index_status', { workspace_id: 'w1' }],
      ['index_open', { workspace_id: 'w1' }],
      ['index_enable', { workspace_id: 'w1' }],
      ['index_disable', { workspace_id: 'w1' }],
      ['index_refresh', { workspace_id: 'w1' }],
      ['index_refresh', { workspace_id: 'w1', full: true }],
      ['index_cancel', { workspace_id: 'w1' }],
      ['index_remove', { workspace_id: 'w1', confirm: true }],
    ]);
  });

  it('rejects with the host code', async () => {
    const ipc = createIndexIpc(vi.fn().mockRejectedValue({ code: 'local_index_disabled', message: 'off by policy' }), noListen);
    await expect(ipc.status('w1')).rejects.toMatchObject({ name: 'WorkspaceIpcError', code: 'local_index_disabled', message: 'off by policy' });
  });

  it('listens on index://event and hands over the payload', async () => {
    const event: IndexEvent = { workspace_id: 'w1', phase: 'ready', message: null, status: { ...OFF_STATUS, state: 'ready' } };
    const listen = vi.fn<ListenFn>((_channel, handler) => {
      handler(event);
      return Promise.resolve(() => undefined);
    });
    const handler = vi.fn();
    await createIndexIpc(vi.fn(), listen).onEvent(handler);
    expect(listen.mock.calls[0]?.[0]).toBe(INDEX_EVENT_CHANNEL);
    expect(handler).toHaveBeenCalledWith(event);
  });
});

describe('createFakeIndexIpc', () => {
  it('follows the host rules: policy gate, index_off, cancel and remove', async () => {
    const ipc = createFakeIndexIpc();
    await expect(ipc.refresh('w1')).rejects.toMatchObject({ code: 'index_off' });
    expect((await ipc.enable('w1')).state).toBe('building');
    expect(await ipc.cancel('w1')).toBe(true);
    expect(await ipc.cancel('w1')).toBe(false);
    await ipc.remove('w1');
    expect((await ipc.status('w1')).state).toBe('off');

    ipc.setPolicyAllowed(false);
    // The policy off is a status (turning off and removing stay allowed); other commands reject.
    await expect(ipc.status('w1')).resolves.toMatchObject({ state: 'off', policy_off: true, on_disk: false });
    await expect(ipc.enable('w1')).rejects.toBeInstanceOf(WorkspaceIpcError);
    // Turning off and removing are allowed whatever the policy says.
    await expect(ipc.disable('w1')).resolves.toMatchObject({ state: 'off' });
    await expect(ipc.remove('w1')).resolves.toBeUndefined();
  });
});
