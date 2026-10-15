import { describe, expect, it, vi } from 'vitest';

import { createHandoffIpc, handoffAllowedBy, HANDOFF_EVENT_CHANNEL, HandoffIpcError, type HandoffEvent } from './handoffIpc';
import { createFakeHandoffIpc } from './handoffIpc.fake';
import { WorkspaceIpcError, type ListenFn } from './workspaceIpc';

const noListen: ListenFn = () => Promise.resolve(() => undefined);

describe('createHandoffIpc', () => {
  it('maps every command to its host name and snake_case arguments', async () => {
    const invoke = vi.fn().mockResolvedValue(null);
    const ipc = createHandoffIpc(invoke, noListen);

    await ipc.preview('w1', 'u1');
    await ipc.preview('w1', 'u1', ['.env.local']);
    await ipc.send('w1', 'u1', ['a.txt']);
    await ipc.status('u1');
    await ipc.status('u1', 42);
    await ipc.reclaim('u1');
    await ipc.preflight('u1');
    await ipc.apply('pf-1', 'worktree');
    await ipc.undo('w1:7');
    await ipc.erase('u1');
    await ipc.erase('u1', 42);

    expect(invoke.mock.calls).toEqual([
      ['handoff_preview', { workspace_id: 'w1', conversation_uuid: 'u1' }],
      ['handoff_preview', { workspace_id: 'w1', conversation_uuid: 'u1', leave_on_laptop: ['.env.local'] }],
      ['handoff_send', { workspace_id: 'w1', conversation_uuid: 'u1', leave_on_laptop: ['a.txt'] }],
      ['handoff_status', { conversation_uuid: 'u1' }],
      ['handoff_status', { conversation_uuid: 'u1', project_id: 42 }],
      ['handoff_reclaim', { conversation_uuid: 'u1' }],
      ['handoff_preflight', { conversation_uuid: 'u1' }],
      ['handoff_apply', { preflight_id: 'pf-1', mode: 'worktree' }],
      ['handoff_undo', { checkpoint_id: 'w1:7' }],
      ['handoff_erase', { conversation_uuid: 'u1' }],
      ['handoff_erase', { conversation_uuid: 'u1', project_id: 42 }],
    ]);
  });

  it('rejects with the host code, message and details', async () => {
    const findings = [{ path: '.env', line: 3, rule: 'aws-key' }];
    const ipc = createHandoffIpc(vi.fn().mockRejectedValue({ code: 'secret_found', message: 'a secret', details: { findings } }), noListen);
    const error = await ipc.send('w1', 'u1', []).catch((cause: unknown) => cause);
    expect(error).toBeInstanceOf(HandoffIpcError);
    // A WorkspaceIpcError too: the screens' shared error handling still reads `code`.
    expect(error).toBeInstanceOf(WorkspaceIpcError);
    expect(error).toMatchObject({ code: 'secret_found', message: 'a secret', details: { findings } });
  });

  it('reads whether a send is allowed from the stored device policy (local work and the handoff switch)', async () => {
    const policy = (local_work: unknown) => createHandoffIpc(vi.fn().mockResolvedValue({ policy: { local_work } }), noListen).allowed();
    await expect(policy({ allowed: true, cloud_handoff: true })).resolves.toBe(true);
    await expect(policy({ allowed: true, cloud_handoff: false })).resolves.toBe(false);
    await expect(policy({ allowed: false, cloud_handoff: true })).resolves.toBe(false);
    await expect(policy({ allowed: true })).resolves.toBe(false);
    expect(handoffAllowedBy(null)).toBe(false);
    expect(handoffAllowedBy({})).toBe(false);
  });

  it('listens on handoff://event and hands over the payload', async () => {
    const event: HandoffEvent = { conversation_uuid: 'u1', phase: 'upload', done: 3, total: 9, message: null };
    const listen = vi.fn<ListenFn>((_channel, handler) => {
      handler(event);
      return Promise.resolve(() => undefined);
    });
    const handler = vi.fn();
    await createHandoffIpc(vi.fn(), listen).onEvent(handler);
    expect(listen.mock.calls[0]?.[0]).toBe(HANDOFF_EVENT_CHANNEL);
    expect(handler).toHaveBeenCalledWith(event);
  });
});

describe('createFakeHandoffIpc', () => {
  it('moves a thread to the cloud on send and back to local on erase', async () => {
    const ipc = createFakeHandoffIpc();
    await ipc.send('w1', 'u1', []);
    expect(await ipc.status('u1')).toMatchObject({ state: 'CLOUD_IDLE', executor: 'cloud', read_only: true });
    await ipc.erase('u1');
    expect(await ipc.status('u1')).toMatchObject({ state: 'LOCAL', executor: 'local' });
  });

  it('rejects the next call the way the host does', async () => {
    const ipc = createFakeHandoffIpc();
    ipc.failNext('apply', 'integrity_failed', 'bad', { x: 1 });
    await expect(ipc.apply('pf', 'apply')).rejects.toMatchObject({ code: 'integrity_failed', details: { x: 1 } });
    await expect(ipc.apply('pf', 'apply')).resolves.toMatchObject({ outcome: 'applied' });
  });
});
