import { describe, expect, it, vi } from 'vitest';

import { createProcessIpc, PROCESS_EVENT_CHANNEL, type ProcessEvent } from './processIpc';
import { plainText } from './processTypes';
import { WorkspaceIpcError, type ListenFn } from './workspaceIpc';

const noListen: ListenFn = () => Promise.resolve(() => undefined);

describe('createProcessIpc', () => {
  it('maps every command to its host name and snake_case arguments', async () => {
    const invoke = vi.fn().mockResolvedValue(null);
    const ipc = createProcessIpc(invoke, noListen);

    await ipc.list('w1');
    await ipc.read('w1', 'p_1');
    await ipc.read('w1', 'p_1', 42);
    await ipc.write('w1', 'p_1', '1+1\n');
    await ipc.resize('w1', 'p_1', 30, 100);
    await ipc.kill('w1', 'p_1');
    await ipc.pin('w1', 'p_1', true);

    expect(invoke.mock.calls).toEqual([
      ['process_list', { workspace_id: 'w1' }],
      ['process_read', { workspace_id: 'w1', process_id: 'p_1' }],
      ['process_read', { workspace_id: 'w1', process_id: 'p_1', since: 42 }],
      ['process_write', { workspace_id: 'w1', process_id: 'p_1', data: '1+1\n' }],
      ['process_resize', { workspace_id: 'w1', process_id: 'p_1', rows: 30, cols: 100 }],
      ['process_kill', { workspace_id: 'w1', process_id: 'p_1' }],
      ['process_pin', { workspace_id: 'w1', process_id: 'p_1', pinned: true }],
    ]);
  });

  it('rejects with the host code', async () => {
    const ipc = createProcessIpc(vi.fn().mockRejectedValue({ code: 'process_unknown', message: 'gone' }), noListen);
    const error: unknown = await ipc.kill('w1', 'p_9').catch((caught: unknown) => caught);
    expect(error).toBeInstanceOf(WorkspaceIpcError);
    expect((error as WorkspaceIpcError).code).toBe('process_unknown');
  });

  it('listens on process://event and hands the payload over', async () => {
    const listen = vi.fn<ListenFn>((_channel, handler) => {
      handler({ workspace_id: 'w1', seq: 0, kind: 'process_output', payload: { process_id: 'p_1', stream: 'pty', data: 'hi', offset: 0 } });
      return Promise.resolve(() => undefined);
    });
    const received: ProcessEvent[] = [];
    await createProcessIpc(vi.fn(), listen).onEvent((event) => received.push(event));
    expect(listen.mock.calls[0]?.[0]).toBe(PROCESS_EVENT_CHANNEL);
    expect(received.map((event) => event.kind)).toEqual(['process_output']);
  });
});

describe('plainText', () => {
  it('drops escape codes and keeps what a carriage return redrew', () => {
    expect(plainText('\u001b[1;32mok\u001b[0m\r\nnext')).toBe('ok\nnext');
    expect(plainText('\u001b]0;title\u0007>>> 2')).toBe('>>> 2');
    expect(plainText('10%\r50%\r100%\ndone')).toBe('100%\ndone');
  });
});
