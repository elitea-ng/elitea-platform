/**
 * The typed client for the person's local MCP servers (#1230; IPC.md
 * "Local MCP servers"): the `mcp_*` commands and the `mcp://event` channel
 * (a repository's proposal, shown when a folder is opened). Wire shapes are
 * the host's (snake_case) and are not renamed here; failures reject with a
 * `WorkspaceIpcError`.
 */
import type { HostInvoke } from './hostBridge';
import { tauriListen, toWorkspaceIpcError, type ListenFn, type TauriEventInternals } from './workspaceIpc';

/** Which settings file: every workspace's, or one workspace's. */
export type McpScope = 'user' | 'workspace';

export interface McpServerConfig {
  command: string;
  args?: string[];
  env?: Record<string, string>;
  cwd?: 'workspace' | 'none';
  network?: boolean;
  enabled?: boolean;
  allowed_tools?: string[] | null;
  read_only_tools?: string[];
  trust_read_only_hints?: boolean;
  source?: string;
}

export interface McpServerStatus {
  name: string;
  scope: McpScope;
  state: 'idle' | 'starting' | 'ready' | 'failed' | 'stopped';
  tools: string[];
  error?: string;
  stderr_tail: string;
  enforcement?: 'full' | 'partial' | 'none' | 'container';
  network: boolean;
  sandbox?: 'read-only' | 'workspace-write' | 'full-access';
}

export interface McpFileView {
  path: string;
  servers: Record<string, McpServerConfig>;
  /** Why the file is not used: it adds no server. */
  error?: string;
}

export interface McpProposedServer {
  name: string;
  command: string;
  args: string[];
  env_keys: string[];
  /** Each value masked: two characters and its length. */
  env_preview: Record<string, string>;
  /** Why it cannot be accepted as written. */
  problem?: string;
}

export interface McpProposal {
  file: string;
  file_sha256: string;
  servers: McpProposedServer[];
}

export interface McpList {
  /** The organisation's policy lets local MCP servers run. */
  allowed: boolean;
  user?: McpFileView;
  workspace?: McpFileView;
  status: McpServerStatus[];
  proposals: McpProposal[];
}

/** `mcp://event`: a folder's unanswered proposal. */
export interface McpProposalEvent extends McpProposal {
  kind: 'mcp_proposal';
  workspace_id: string;
}

export interface McpIpc {
  list(workspaceId: string): Promise<McpList>;
  save(scope: McpScope, name: string, server: McpServerConfig, workspaceId: string): Promise<void>;
  remove(scope: McpScope, name: string, workspaceId: string): Promise<boolean>;
  restart(workspaceId: string, name: string): Promise<McpServerStatus>;
  /** Copy the named servers (none: declined) into the workspace's settings. */
  answerProposal(workspaceId: string, fileSha256: string, accept: string[]): Promise<{ accepted: string[] }>;
  /** Subscribe to `mcp://event`; resolves once the subscription is live, with its unsubscribe. */
  onProposal(handler: (event: McpProposalEvent) => void): Promise<() => void>;
}

export const MCP_EVENT_CHANNEL = 'mcp://event';

export function createMcpIpc(hostInvoke: HostInvoke, listen: ListenFn): McpIpc {
  const invoke = <T>(command: string, args: Record<string, unknown>): Promise<T> =>
    hostInvoke<T>(command, args).catch((error: unknown) => {
      throw toWorkspaceIpcError(error);
    });
  // A user-scope change names the workspace too: the host checks it is known.
  const target = (scope: McpScope, name: string, workspaceId: string): Record<string, unknown> => ({ scope, name, workspace_id: workspaceId });
  return {
    list: (workspaceId) => invoke<McpList>('mcp_list', { workspace_id: workspaceId }),
    save: (scope, name, server, workspaceId) => invoke<void>('mcp_save', { ...target(scope, name, workspaceId), server }),
    remove: (scope, name, workspaceId) => invoke<boolean>('mcp_remove', target(scope, name, workspaceId)),
    restart: (workspaceId, name) => invoke<McpServerStatus>('mcp_restart', { workspace_id: workspaceId, name }),
    answerProposal: (workspaceId, fileSha256, accept) =>
      invoke<{ accepted: string[] }>('mcp_proposal_answer', { workspace_id: workspaceId, file_sha256: fileSha256, accept }),
    onProposal: (handler) =>
      listen(MCP_EVENT_CHANNEL, (payload) => {
        const event = payload as McpProposalEvent;
        if (event.kind === 'mcp_proposal') handler(event);
      }),
  };
}

/** The real client, or `undefined` outside the Tauri webview. */
export function tauriMcpIpc(): McpIpc | undefined {
  const internals = (globalThis as { __TAURI_INTERNALS__?: Partial<TauriEventInternals> }).__TAURI_INTERNALS__;
  if (internals?.invoke === undefined || internals.transformCallback === undefined) return undefined;
  const bound: TauriEventInternals = {
    invoke: internals.invoke.bind(internals),
    transformCallback: internals.transformCallback.bind(internals),
  };
  return createMcpIpc(bound.invoke, tauriListen(bound));
}
