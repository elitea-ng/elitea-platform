import { describe, expect, it, vi } from 'vitest';

import { APP_COMMAND_CHANNEL, createAppIpc, tauriAppIpc, type AppCommand } from './appEvents';
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
    expect(invoke.mock.calls).toEqual([
      ['app_platform'],
      ['reveal_path', { workspace_id: 'w1', path: 'src/main.rs' }],
      ['open_path', { workspace_id: 'w1', path: '' }],
      ['plugin:window|set_theme', { label: 'main', value: 'dark' }],
      ['plugin:window|set_theme', { label: 'main', value: null }],
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
  });
});
