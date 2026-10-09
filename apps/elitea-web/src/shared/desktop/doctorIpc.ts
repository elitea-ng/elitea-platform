/**
 * The typed client for the host's Doctor (IPC.md, `doctor_run` /
 * `doctor_fix`; README, "Diagnostics"): checks of what the app keeps on this
 * computer and needs from the deployment, each with the repair the host can
 * apply. Wire shapes are the host's (snake_case). Failures reject with a
 * `WorkspaceIpcError`.
 */
import type { HostInvoke } from './hostBridge';
import { tauriInvoke } from './hostBridge';
import { toWorkspaceIpcError } from './workspaceIpc';

export type DoctorStatus = 'ok' | 'warn' | 'fail';

export interface DoctorCheck {
  id: string;
  title: string;
  status: DoctorStatus;
  /** For a person; never a secret. */
  message: string;
  /** The repair `fix` takes, when the host has one. */
  fix_id?: string;
  fix_label?: string;
}

export interface DoctorIpc {
  /** `local`: this computer's files only (no network) — what the launch notice uses. */
  run(scope?: 'local'): Promise<DoctorCheck[]>;
  /** Apply one repair; what happened, for a person. */
  fix(fixId: string): Promise<string>;
}

export function createDoctorIpc(invoke: HostInvoke): DoctorIpc {
  const call = <T>(command: string, args: Record<string, unknown>): Promise<T> =>
    invoke<T>(command, args).catch((error: unknown) => {
      throw toWorkspaceIpcError(error);
    });
  return {
    run: (scope) => call<DoctorCheck[]>('doctor_run', scope === undefined ? {} : { scope }),
    fix: (fixId) => call<{ message: string }>('doctor_fix', { fix_id: fixId }).then((outcome) => outcome.message),
  };
}

/** The real client, or `undefined` outside the Tauri webview. */
export function tauriDoctorIpc(): DoctorIpc | undefined {
  const invoke = tauriInvoke();
  return invoke === undefined ? undefined : createDoctorIpc(invoke);
}

/** The launch notice shows when a check failed (a warning alone does not interrupt). */
export function needsAttention(checks: readonly DoctorCheck[]): boolean {
  return checks.some((check) => check.status === 'fail');
}
