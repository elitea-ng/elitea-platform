import { describe, expect, it, vi } from 'vitest';

import { APP_COMMAND_CHANNEL, CONTEXT_MENU_CHANNEL, createAppIpc, tauriAppIpc, type AppCommand } from './appEvents';
import { createFakeAppIpc, MACOS_PLATFORM } from './appEvents.fake';
import { WorkspaceIpcError, type ListenFn } from './workspaceIpc';

describe('createAppIpc', () => {
  it('maps the commands to their host names and snake_case arguments', async () => {
    const invoke = vi.fn().mockResolvedValue(null);
    const ipc = createAppIpc(invoke, () => Promise.resolve(() => undefined));
    await ipc.platform();
    await ipc.revealPath('w1', 'src/main.rs');
    await ipc.openPath('w1', '');
    await ipc.setWindowTheme('dark');
    await ipc.setWindowTheme(null);
    await ipc.focusWindow('panel-changes');
    await ipc.ready();
    expect(invoke.mock.calls).toEqual([
      ['app_platform'],
      ['reveal_path', { workspace_id: 'w1', path: 'src/main.rs' }],
      ['open_path', { workspace_id: 'w1', path: '' }],
      ['plugin:window|set_theme', { label: 'main', value: 'dark' }],
      ['plugin:window|set_theme', { label: 'main', value: null }],
      ['plugin:window|set_focus', { label: 'panel-changes' }],
      ['app_ready'],
    ]);
  });

  it('rejects with the host code', async () => {
    const invoke = vi.fn().mockRejectedValue({ code: 'open_refused', message: 'no' });
    const ipc = createAppIpc(invoke, () => Promise.resolve(() => undefined));
    await expect(ipc.openPath('w1', 'run.command')).rejects.toEqual(new WorkspaceIpcError('open_refused', 'no'));
  });

  it('listens on app://command and hands the payload over', async () => {
    let deliver: ((payload: unknown) => void) | undefined;
    const listen: ListenFn = (channel, handler) => {
      expect(channel).toBe(APP_COMMAND_CHANNEL);
      deliver = handler;
      return Promise.resolve(() => undefined);
    };
    const received: AppCommand[] = [];
    await createAppIpc(vi.fn(), listen).onCommand((command) => received.push(command));
    deliver?.({ id: 'workspace_opened', args: { workspace_id: 'abc' } });
    deliver?.({ id: 'toggle_sidebar' });
    expect(received).toEqual([{ id: 'workspace_opened', args: { workspace_id: 'abc' } }, { id: 'toggle_sidebar' }]);
  });

  it('pops a native context menu and resolves with the outcome the host sent for THIS request', async () => {
    let deliver: ((payload: unknown) => void) | undefined;
    const off = vi.fn();
    const listen: ListenFn = (channel, handler) => {
      expect(channel).toBe(CONTEXT_MENU_CHANNEL);
      deliver = handler;
      return Promise.resolve(off);
    };
    const items = [{ id: 'open', label: 'Open' }, { separator: true as const }, { label: 'Project', children: [{ id: 'p:1', label: 'One' }] }];
    let requestId = '';
    const shown = vi.fn().mockImplementation((_command: string, args: { request_id: string }) => {
      requestId = args.request_id;
      // A stray outcome of another menu is ignored; the matching one answers.
      deliver?.({ request_id: 'someone-else', id: 'rm' });
      deliver?.({ request_id: requestId, id: 'open' });
      return Promise.resolve(null);
    });
    expect(await createAppIpc(shown, listen).contextMenu(items)).toBe('open');
    expect(shown).toHaveBeenCalledWith('context_menu_show', { request_id: requestId, items });
    expect(requestId.length).toBeGreaterThan(8);
    expect(off).toHaveBeenCalledTimes(1);
    // Dismissed: the host says so, explicitly.
    const dismissed = vi.fn().mockImplementation((_command: string, args: { request_id: string }) => {
      deliver?.({ request_id: args.request_id, id: null });
      return Promise.resolve(null);
    });
    expect(await createAppIpc(dismissed, listen).contextMenu(items)).toBeNull();
    // A refusal rejects (and still stops listening).
    const refused = vi.fn().mockRejectedValue({ code: 'unknown', message: 'a context menu has at most 60 items' });
    await expect(createAppIpc(refused, listen).contextMenu(items)).rejects.toThrow('at most 60');
    expect(off).toHaveBeenCalledTimes(3);
  });

  it('tells the host a cloud turn started and ended', async () => {
    const invoke = vi.fn().mockResolvedValue(null);
    const ipc = createAppIpc(invoke, () => Promise.resolve(() => undefined));
    await ipc.cloudRun({ conversationUuid: 'u1', running: true, waiting: false, workspaceId: 'w1', workspaceName: 'app' });
    await ipc.cloudRun({ conversationUuid: 'u1', running: false, waiting: true });
    expect(invoke.mock.calls).toEqual([
      ['activity_cloud_run', { conversation_uuid: 'u1', running: true, waiting: false, workspace_id: 'w1', workspace_name: 'app' }],
      ['activity_cloud_run', { conversation_uuid: 'u1', running: false, waiting: true }],
    ]);
  });

  it('is undefined outside the Tauri webview', () => {
    expect(tauriAppIpc()).toBeUndefined();
  });
});

describe('createFakeAppIpc', () => {
  it('plays the host side', async () => {
    const fake = createFakeAppIpc(MACOS_PLATFORM);
    expect((await fake.platform()).traffic_light_inset_px).toBe(90);
    const received: AppCommand[] = [];
    const stop = await fake.onCommand((command) => received.push(command));
    fake.emit({ id: 'files_dropped', args: { paths: ['/tmp/a.txt'] } });
    stop();
    fake.emit({ id: 'back' });
    expect(received).toEqual([{ id: 'files_dropped', args: { paths: ['/tmp/a.txt'] } }]);
    fake.failNext('revealPath', 'not_found', 'gone');
    await expect(fake.revealPath('w', 'x')).rejects.toBeInstanceOf(WorkspaceIpcError);
    await fake.revealPath('w', 'x');
    expect(fake.calls.revealed).toHaveLength(2);
    fake.chooseFromContextMenu('open');
    expect(await fake.contextMenu([{ id: 'open', label: 'Open' }])).toBe('open');
    expect(await fake.contextMenu([{ id: 'open', label: 'Open' }])).toBeNull();
    expect(fake.calls.contextMenus).toHaveLength(2);
  });
});
