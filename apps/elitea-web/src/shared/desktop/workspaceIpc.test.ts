import { afterEach, describe, expect, it, vi } from 'vitest';

import { createWorkspaceIpc, tauriWorkspaceIpc, WorkspaceIpcError, type AgentEvent, type ListenFn } from './workspaceIpc';
import { createFakeWorkspaceIpc } from './workspaceIpc.fake';

const noListen: ListenFn = () => Promise.resolve(() => undefined);

describe('createWorkspaceIpc', () => {
  it('maps every command to its host name and snake_case arguments', async () => {
    const invoke = vi.fn().mockResolvedValue(null);
    const ipc = createWorkspaceIpc(invoke, noListen);

    await ipc.open();
    await ipc.list();
    await ipc.remove('w1');
    await ipc.bindProject('w1', 7);
    await ipc.startTurn({
      workspace_id: 'w1',
      project_id: 7,
      conversation_id: 'c1',
      application_id: 3,
      version_id: 9,
      prompt: 'hi',
      plan_mode: true,
    });
    await ipc.cancelTurn('t1');
    await ipc.turnStatus('t1');
    await ipc.respondApproval('r1', 'allow_always');
    await ipc.turnChanges('t1');
    await ipc.restore('t1');
    await ipc.restore('t1', 'a/b.txt');

    expect(invoke.mock.calls).toEqual([
      ['workspace_open'],
      ['workspace_list'],
      ['workspace_remove', { id: 'w1' }],
      ['workspace_bind_project', { id: 'w1', project_id: 7 }],
      ['agent_turn_start', { workspace_id: 'w1', project_id: 7, conversation_id: 'c1', application_id: 3, version_id: 9, prompt: 'hi', plan_mode: true }],
      ['agent_turn_cancel', { turn_id: 't1' }],
      ['agent_turn_status', { turn_id: 't1' }],
      ['approval_respond', { request_id: 'r1', decision: 'allow_always' }],
      ['turn_changes', { turn_id: 't1' }],
      ['checkpoint_restore', { turn_id: 't1' }],
      ['checkpoint_restore', { turn_id: 't1', path: 'a/b.txt' }],
    ]);
  });

  it("rejects with the host's {code, message} as a WorkspaceIpcError", async () => {
    const invoke = vi.fn().mockRejectedValueOnce({ code: 'workspace_busy', message: 'A turn is already running in this workspace.' });
    const ipc = createWorkspaceIpc(invoke, noListen);
    const error: unknown = await ipc.remove('w1').catch((e: unknown) => e);
    expect(error).toBeInstanceOf(WorkspaceIpcError);
    expect(error).toMatchObject({ code: 'workspace_busy', message: 'A turn is already running in this workspace.' });

    invoke.mockRejectedValueOnce('a plain host string');
    await expect(ipc.list()).rejects.toMatchObject({ code: 'unknown', message: 'a plain host string' });
  });

  it('subscribes to agent://event and hands each payload to the handler', async () => {
    let deliver: ((payload: unknown) => void) | undefined;
    const listen: ListenFn = (channel, handler) => {
      expect(channel).toBe('agent://event');
      deliver = handler;
      return Promise.resolve(() => undefined);
    };
    const ipc = createWorkspaceIpc(vi.fn(), listen);
    const seen: AgentEvent[] = [];
    await ipc.onEvent((event) => seen.push(event));

    const event: AgentEvent = { turn_id: 't', seq: 1, kind: 'text_delta', payload: { text: 'x' } };
    deliver?.(event);
    expect(seen).toEqual([event]);
  });
});

describe('tauriWorkspaceIpc', () => {
  afterEach(() => {
    delete (globalThis as { __TAURI_INTERNALS__?: unknown }).__TAURI_INTERNALS__;
  });

  it('is undefined outside the Tauri webview', () => {
    expect(tauriWorkspaceIpc()).toBeUndefined();
  });

  it('listens through the event plugin and unlistens with the id it was given', async () => {
    const callbacks: ((envelope: { event: string; id: number; payload: unknown }) => void)[] = [];
    const invoke = vi.fn((command: string) => Promise.resolve(command === 'plugin:event|listen' ? 42 : undefined));
    (globalThis as { __TAURI_INTERNALS__?: unknown }).__TAURI_INTERNALS__ = {
      invoke,
      transformCallback: (callback: (envelope: { event: string; id: number; payload: unknown }) => void) => callbacks.push(callback),
    };

    const ipc = tauriWorkspaceIpc();
    const seen: AgentEvent[] = [];
    const off = await ipc?.onEvent((event) => seen.push(event));

    expect(invoke).toHaveBeenCalledWith('plugin:event|listen', { event: 'agent://event', target: { kind: 'Any' }, handler: 1 });
    const event: AgentEvent = { turn_id: 't', seq: 1, kind: 'error', payload: { code: 'x', message: 'boom' } };
    callbacks[0]?.({ event: 'agent://event', id: 42, payload: event });
    expect(seen).toEqual([event]);

    off?.();
    expect(invoke).toHaveBeenCalledWith('plugin:event|unlisten', { event: 'agent://event', eventId: 42 });
  });
});

describe('createFakeWorkspaceIpc', () => {
  it('behaves like the host for the workspace commands and records turn calls', async () => {
    const folder = { id: 'w1', path: '/tmp/a', name: 'a', project_id: null, is_git: true };
    const ipc = createFakeWorkspaceIpc({ nextOpen: folder });

    expect(await ipc.open()).toEqual(folder);
    await ipc.bindProject('w1', 5);
    expect((await ipc.list())[0]?.project_id).toBe(5);
    await ipc.remove('w1');
    expect(await ipc.list()).toEqual([]);

    ipc.setChanges('t', { files: [{ path: 'f', status: 'added', added: 1, removed: 0, diff: '' }] });
    expect(await ipc.restore('t')).toEqual({ restored: ['f'] });
    expect(ipc.calls.restores).toEqual([{ turnId: 't' }]);

    ipc.failNext('restore', 'workspace_busy', 'busy');
    await expect(ipc.restore('t')).rejects.toMatchObject({ code: 'workspace_busy' });
    expect(await ipc.restore('t')).toEqual({ restored: ['f'] });
  });

  it('delivers emitted events to subscribers until they unsubscribe', async () => {
    const ipc = createFakeWorkspaceIpc();
    const seen: number[] = [];
    const off = await ipc.onEvent((event) => seen.push(event.seq));
    ipc.emit({ turn_id: 't', seq: 1, kind: 'text_delta', payload: { text: '' } });
    off();
    ipc.emit({ turn_id: 't', seq: 2, kind: 'text_delta', payload: { text: '' } });
    expect(seen).toEqual([1]);
    expect(ipc.subscriberCount()).toBe(0);
  });
});
