/**
 * The desktop page's boot, by the webview it runs in (`./main.tsx` calls
 * [`boot`] with the real pieces).
 *
 * The main window boots the shell: the host's log (`plugin:log|log`), the
 * native menus' labels (`app_menu_labels_set`) and the signed-in app. The
 * computer use session panel (`index.html#computer-panel`, ADR-0047
 * decision 6) is its own webview whose capability grants only
 * `computer_session` and event listening: it calls nothing else, so it
 * neither logs to the host nor sets the menus' labels, and it loads the
 * panel alone, not the shell.
 */
import type { HostInvoke } from '@/shared/desktop/hostBridge';

/** The panel's hash in the bundled UI (`computer_commands::build_panel`). */
export const COMPUTER_PANEL_HASH = '#computer-panel';

export interface BootPlan {
  /** Forward the webview's errors to the host's log. */
  hostLog: boolean;
  /** Give the host's native menus their labels in the page's language. */
  menuLabels: boolean;
  /** Which page to render. */
  page: 'shell' | 'computer-panel';
}

export function bootPlan(hash: string): BootPlan {
  if (hash === COMPUTER_PANEL_HASH) return { hostLog: false, menuLabels: false, page: 'computer-panel' };
  return { hostLog: true, menuLabels: true, page: 'shell' };
}

/** What the boot needs: the host, and each step it may take. */
export interface BootSteps {
  invoke: HostInvoke | undefined;
  installHostLog: (invoke: HostInvoke) => void;
  log: (level: 'info' | 'warn' | 'error', message: string) => void;
  /** Apply the language (catalogues, right-to-left styles). */
  applyLanguage: () => Promise<void>;
  menuLabels: () => Record<string, string>;
  startShell: (invoke: HostInvoke | undefined) => Promise<void>;
  startPanel: () => Promise<void>;
}

/** Boot the page `hash` names. Resolves once its page started. */
export async function boot(hash: string, steps: BootSteps): Promise<void> {
  const plan = bootPlan(hash);
  const { invoke } = steps;
  const log = plan.hostLog ? steps.log : (): void => undefined;
  // First, so an error anywhere after this reaches the host's log file.
  if (invoke !== undefined && plan.hostLog) {
    steps.installHostLog(invoke);
    log('info', `webview started (${import.meta.env.MODE})`);
  }
  try {
    await steps.applyLanguage();
    if (invoke !== undefined && plan.menuLabels) {
      // Every time, English too: the host keeps the last labels it was given until it restarts.
      void invoke('app_menu_labels_set', { labels: steps.menuLabels() }).catch((error: unknown) =>
        log('warn', `menu labels not applied: ${String(error)}`),
      );
    }
  } catch (error) {
    // A language that fails to load leaves English; the page starts either way.
    log('error', `language not applied: ${String(error)}`);
  }
  try {
    await (plan.page === 'computer-panel' ? steps.startPanel() : steps.startShell(invoke));
  } catch (error) {
    log('error', `shell failed to start: ${String(error)}`);
  }
}
