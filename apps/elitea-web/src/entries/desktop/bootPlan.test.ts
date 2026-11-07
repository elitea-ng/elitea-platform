import { describe, expect, it, vi } from 'vitest';

import { boot, bootPlan, COMPUTER_PANEL_HASH, type BootSteps } from './bootPlan';

function steps(): BootSteps & { invoked: string[] } {
  const invoked: string[] = [];
  return {
    invoked,
    invoke: <T>(command: string): Promise<T> => {
      invoked.push(command);
      return Promise.resolve(null as T);
    },
    installHostLog: vi.fn(),
    log: vi.fn(),
    applyLanguage: vi.fn(() => Promise.resolve()),
    menuLabels: () => ({ tray_quit: 'Quit Elitea' }),
    startShell: vi.fn(() => Promise.resolve()),
    startPanel: vi.fn(() => Promise.resolve()),
  };
}

describe('the desktop boot', () => {
  it('names the page and what it calls by the webview', () => {
    expect(bootPlan('')).toEqual({ hostLog: true, menuLabels: true, page: 'shell' });
    expect(bootPlan('#/workspaces').page).toBe('shell');
    expect(bootPlan(COMPUTER_PANEL_HASH)).toEqual({ hostLog: false, menuLabels: false, page: 'computer-panel' });
  });

  it('boots the shell with the host log and the menu labels in the main window', async () => {
    const host = steps();
    await boot('', host);
    expect(host.installHostLog).toHaveBeenCalledOnce();
    expect(host.invoked).toEqual(['app_menu_labels_set']);
    expect(host.startShell).toHaveBeenCalledWith(host.invoke);
    expect(host.startPanel).not.toHaveBeenCalled();
  });

  it('starts the session panel alone, calling nothing its capability does not grant', async () => {
    const host = steps();
    await boot(COMPUTER_PANEL_HASH, host);
    expect(host.startPanel).toHaveBeenCalledOnce();
    expect(host.startShell).not.toHaveBeenCalled();
    expect(host.installHostLog).not.toHaveBeenCalled();
    expect(host.log).not.toHaveBeenCalled();
    expect(host.invoked).toEqual([]);
  });

  it('starts the page when the language fails, and logs it in the main window only', async () => {
    const shell = steps();
    shell.applyLanguage = () => Promise.reject(new Error('no catalogue'));
    await boot('', shell);
    expect(shell.startShell).toHaveBeenCalledOnce();
    expect(shell.log).toHaveBeenCalledWith('error', 'language not applied: Error: no catalogue');

    const panel = steps();
    panel.applyLanguage = () => Promise.reject(new Error('no catalogue'));
    panel.startPanel = () => Promise.reject(new Error('no root'));
    await boot(COMPUTER_PANEL_HASH, panel);
    expect(panel.log).not.toHaveBeenCalled();
    expect(panel.invoked).toEqual([]);
  });
});
