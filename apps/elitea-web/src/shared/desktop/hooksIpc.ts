/**
 * The typed client for the person's hooks (#1237; IPC.md "Hooks"): the
 * `hooks_*` commands and the `hooks://event` channel (a repository's
 * proposal, shown when a folder is opened). Wire shapes are the host's
 * (snake_case) and are not renamed here; failures reject with a
 * `WorkspaceIpcError`.
 */
import type { HostInvoke } from './hostBridge';
import { tauriListen, toWorkspaceIpcError, type ListenFn, type TauriEventInternals } from './workspaceIpc';

/** Which hooks file: every workspace's, or one workspace's. */
export type HooksScope = 'user' | 'workspace';

type HookEvent = 'PreToolUse' | 'PostToolUse' | 'Stop' | 'UserPromptSubmit';

export interface Hook {
  id: string;
  event: HookEvent;
  match?: { tools?: string[]; paths?: string[] };
  command: string[];
  timeout_ms?: number;
  sandbox?: 'read-only' | 'workspace-write';
  network?: boolean;
  fail_closed?: boolean;
  enabled?: boolean;
  source?: string;
}

export interface HookRun {
  id: string;
  event: HookEvent;
  result: 'ok' | 'deny' | 'ask' | 'modified' | 'annotated' | 'failed' | 'timed_out';
  duration_ms: number;
  at_ms: number;
  /** For the person: why it failed, or what it wrote. */
  message?: string;
}

export interface HooksFileView {
  path: string;
  hooks: Hook[];
  /** Why the file is not used: it adds no hook. */
  error?: string;
}

export interface ProposedHook extends Hook {
  /** Why it cannot be trusted as written. */
  problem?: string;
}

export interface HooksProposal {
  file: string;
  file_sha256: string;
  hooks: ProposedHook[];
}

export interface HooksList {
  /** The organisation's policy lets hooks run (local work and the shell on). */
  allowed: boolean;
  user?: HooksFileView;
  workspace?: HooksFileView;
  last_runs: HookRun[];
  proposals: HooksProposal[];
}

/** `hooks://event`: a folder's unanswered proposal. */
export interface HooksProposalEvent extends HooksProposal {
  kind: 'hooks_proposal';
  workspace_id: string;
}

export interface HooksIpc {
  list(workspaceId: string): Promise<HooksList>;
  /** Replace the scope's hooks (schema-checked by the host). */
  save(scope: HooksScope, hooks: Hook[], workspaceId: string): Promise<void>;
  setEnabled(scope: HooksScope, id: string, enabled: boolean, workspaceId: string): Promise<boolean>;
  /** Copy the named hooks (none: declined) into the workspace's file. */
  answerProposal(workspaceId: string, fileSha256: string, accept: string[]): Promise<{ accepted: string[] }>;
  /** Subscribe to `hooks://event`; resolves once the subscription is live, with its unsubscribe. */
  onProposal(handler: (event: HooksProposalEvent) => void): Promise<() => void>;
}

export const HOOKS_EVENT_CHANNEL = 'hooks://event';

export function createHooksIpc(hostInvoke: HostInvoke, listen: ListenFn): HooksIpc {
  const invoke = <T>(command: string, args: Record<string, unknown>): Promise<T> =>
    hostInvoke<T>(command, args).catch((error: unknown) => {
      throw toWorkspaceIpcError(error);
    });
  return {
    list: (workspaceId) => invoke<HooksList>('hooks_list', { workspace_id: workspaceId }),
    // A user-scope change names the workspace too: the host checks it is known.
    save: (scope, hooks, workspaceId) => invoke<void>('hooks_save', { scope, hooks, workspace_id: workspaceId }),
    setEnabled: (scope, id, enabled, workspaceId) => invoke<boolean>('hooks_set_enabled', { scope, id, enabled, workspace_id: workspaceId }),
    answerProposal: (workspaceId, fileSha256, accept) =>
      invoke<{ accepted: string[] }>('hooks_proposal_answer', { workspace_id: workspaceId, file_sha256: fileSha256, accept }),
    onProposal: (handler) =>
      listen(HOOKS_EVENT_CHANNEL, (payload) => {
        const event = payload as HooksProposalEvent;
        if (event.kind === 'hooks_proposal') handler(event);
      }),
  };
}

/** The real client, or `undefined` outside the Tauri webview. */
export function tauriHooksIpc(): HooksIpc | undefined {
  const internals = (globalThis as { __TAURI_INTERNALS__?: Partial<TauriEventInternals> }).__TAURI_INTERNALS__;
  if (internals?.invoke === undefined || internals.transformCallback === undefined) return undefined;
  const bound: TauriEventInternals = {
    invoke: internals.invoke.bind(internals),
    transformCallback: internals.transformCallback.bind(internals),
  };
  return createHooksIpc(bound.invoke, tauriListen(bound));
}
