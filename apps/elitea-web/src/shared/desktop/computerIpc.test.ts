import { describe, expect, it, vi } from 'vitest';

import { COMPUTER_EVENT_CHANNEL, createComputerIpc, shortcutLabel, tauriComputerIpc, type SessionView } from './computerIpc';
import { WorkspaceIpcError, type ListenFn } from './workspaceIpc';

const noListen: ListenFn = () => Promise.resolve(() => undefined);

describe('createComputerIpc', () => {
  it('maps every command to its host name and snake_case arguments', async () => {
    const invoke = vi.fn().mockResolvedValue(null);
    const ipc = createComputerIpc(invoke, noListen);

    await ipc.settings();
    await ipc.setEnabled(true);
    await ipc.forgetApp('com.apple.textedit');
    await ipc.openPrivacy('screen_recording');
    await ipc.session('pause');

    expect(invoke.mock.calls).toEqual([
      ['computer_settings_get'],
      ['computer_settings_set', { enabled: true }],
      ['computer_app_forget', { bundle_id: 'com.apple.textedit' }],
      ['computer_open_privacy', { grant: 'screen_recording' }],
      ['computer_session', { action: 'pause' }],
    ]);
  });

  it('rejects with the host code', async () => {
    const ipc = createComputerIpc(vi.fn().mockRejectedValue({ code: 'computer_accessibility_missing', message: 'Grant Accessibility to Elitea first.' }), noListen);
    const error: unknown = await ipc.setEnabled(true).catch((caught: unknown) => caught);
    expect(error).toBeInstanceOf(WorkspaceIpcError);
    expect((error as WorkspaceIpcError).code).toBe('computer_accessibility_missing');
  });

  it('hands over the session changes from computer://event, without the kind', async () => {
    let deliver: (payload: unknown) => void = () => undefined;
    const listen: ListenFn = (channel, handler) => {
      expect(channel).toBe(COMPUTER_EVENT_CHANNEL);
      deliver = handler;
      return Promise.resolve(() => undefined);
    };
    const seen: SessionView[] = [];
    await createComputerIpc(vi.fn(), listen).onSession((view) => seen.push(view));
    deliver({ kind: 'other', phase: 'active' });
    deliver({ kind: 'computer_session', phase: 'paused', paused_by: 'user_active', app: 'TextEdit', action: 'press' });
    expect(seen).toEqual([{ phase: 'paused', paused_by: 'user_active', app: 'TextEdit', action: 'press' }]);
  });

  it('is absent outside the desktop app', () => {
    expect(tauriComputerIpc()).toBeUndefined();
  });
});

describe('shortcutLabel', () => {
  it('writes the stop shortcut as macOS does', () => {
    expect(shortcutLabel('ctrl+option+escape')).toBe('⌃⌥⎋');
    expect(shortcutLabel('cmd+shift+k')).toBe('⌘⇧K');
  });
});
