/**
 * The typed client for per-folder trust (IPC.md, "Trusted folders";
 * ADR-0061 decision 5, #3059 and #3062): whether a folder's commands run
 * outside the OS sandbox as the person, what the organisation's policy
 * allows, and the person's grant and revoke. Wire shapes are the host's
 * (snake_case). Failures reject with a `WorkspaceIpcError` whose `code` is
 * `trust_not_allowed`, `trust_refused` or `confirmation_outdated`.
 */
import type { HostInvoke } from './hostBridge';
import { tauriListen, toWorkspaceIpcError, type ListenFn, type TauriEventInternals } from './workspaceIpc';

type TrustState = 'untrusted' | 'trusted' | 'suspended';
/** Why the policy does not allow trust: local work off, the shell off, the network off, a ceiling below full access, or `local_work_folder_trust` off. */
export type TrustPolicyBlock = 'local_work' | 'shell' | 'network' | 'ceiling' | 'folder_trust';

export interface TrustStatus {
  workspace_id: string;
  state: TrustState;
  suspended_by: 'policy' | 'tamper' | null;
  /** The settings file changed outside the app (`rules.json`), when `suspended_by` is `tamper`. */
  tampered_file: string | null;
  allowed_by_policy: boolean;
  /** Empty when the policy allows trust. */
  policy_blocks: TrustPolicyBlock[];
  /** A task worktree: the root folder whose trust it follows (a grant on it is refused). */
  inherited_from: string | null;
  /** `false` for a task worktree or a folder inside the app's own data. */
  grantable: boolean;
  ask_every_command: boolean;
  /** The login-shell environment could not be read: commands get a reduced one. */
  env_fallback: boolean;
  platform: 'macos' | 'linux' | 'windows';
  /** `false` on Windows: there is no sandbox to fall back to. */
  sandbox_available: boolean;
  granted_at: string | null;
  last_host_command_at: string | null;
  /** The dialog text version the host accepts. */
  confirmation_version: number;
}

export interface TrustEntry extends TrustStatus {
  name: string;
  path: string;
}

/** `trust://event`: a folder's trust changed (`null`: every folder), or a settings file changed outside the app. */
type TrustEvent = { kind: 'changed'; workspace_id: string | null } | { kind: 'tamper'; file: string };

const TRUST_EVENT_CHANNEL = 'trust://event';

export interface TrustIpc {
  status(workspaceId: string): Promise<TrustStatus>;
  grant(workspaceId: string, confirmationVersion: number, askEveryCommand: boolean): Promise<TrustStatus>;
  revoke(workspaceId: string): Promise<TrustStatus>;
  list(): Promise<TrustEntry[]>;
  /** Subscribe to `trust://event`; resolves once the subscription is live, with its unsubscribe. */
  onEvent(handler: (event: TrustEvent) => void): Promise<() => void>;
}

function createTrustIpc(hostInvoke: HostInvoke, listen: ListenFn): TrustIpc {
  const invoke = <T>(command: string, args?: Record<string, unknown>): Promise<T> =>
    hostInvoke<T>(command, args).catch((error: unknown) => {
      throw toWorkspaceIpcError(error);
    });
  return {
    status: (workspaceId) => invoke<TrustStatus>('trust_status', { workspace_id: workspaceId }),
    grant: (workspaceId, confirmationVersion, askEveryCommand) =>
      invoke<TrustStatus>('trust_grant', { workspace_id: workspaceId, confirmation_version: confirmationVersion, ask_every_command: askEveryCommand }),
    revoke: (workspaceId) => invoke<TrustStatus>('trust_revoke', { workspace_id: workspaceId }),
    list: () => invoke<TrustEntry[]>('trust_list'),
    onEvent: (handler) => listen(TRUST_EVENT_CHANNEL, (payload) => handler(payload as TrustEvent)),
  };
}

/** The real client, or `undefined` outside the Tauri webview. */
export function tauriTrustIpc(): TrustIpc | undefined {
  const internals = (globalThis as { __TAURI_INTERNALS__?: Partial<TauriEventInternals> }).__TAURI_INTERNALS__;
  if (internals?.invoke === undefined || internals.transformCallback === undefined) return undefined;
  const bound: TauriEventInternals = {
    invoke: internals.invoke.bind(internals),
    transformCallback: internals.transformCallback.bind(internals),
  };
  return createTrustIpc(bound.invoke, tauriListen(bound));
}
