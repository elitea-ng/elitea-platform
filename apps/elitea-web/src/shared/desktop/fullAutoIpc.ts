/**
 * Whether the desktop host offers Full auto (IPC.md "Full auto", issue 3079):
 * `full_auto_status` answers the organisation's `local_work_full_auto` with
 * the device's managed value applied. Outside the Tauri webview, or on a host
 * without the command, Full auto is not offered.
 */
import type { TauriEventInternals } from './workspaceIpc';

interface FullAutoStatus {
  allowed: boolean;
}

export async function fullAutoAllowed(): Promise<boolean> {
  const internals = (globalThis as { __TAURI_INTERNALS__?: Partial<TauriEventInternals> }).__TAURI_INTERNALS__;
  if (internals?.invoke === undefined) return false;
  try {
    const status = await internals.invoke.call(internals, 'full_auto_status') as Partial<FullAutoStatus> | null;
    return status?.allowed === true;
  } catch {
    return false;
  }
}
