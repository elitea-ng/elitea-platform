/**
 * `mcp_sync_tools` — hand-written port of the request half of
 * `apps/elitea-ui/src/api/toolkits.js:314-342`'s `mcpSyncTools` mutation
 * (baseline domain: toolkits' RTK Query API; consumed here ONLY by this
 * slice's OWN `model/useGetRemoteMcpTools.ts`, which is in A5's
 * `sourceFiles`). Not a toolkits-domain UI/model port — a single request
 * function, same narrow-scope posture as `toolkitCredentials.ts`.
 */
import { eliteaFetch } from '@/shared/api/generated/mutator';

export interface McpSyncToolsParams {
  projectId: string | number;
  /** Remote-MCP server URL. Omitted for pre-built MCPs — the backend resolves it from `toolkit_type`. */
  url?: string | undefined;
  headers?: Readonly<Record<string, string>> | undefined;
  timeout?: number | undefined;
  mcp_tokens?: Readonly<Record<string, unknown>> | undefined;
  /** Current socket connection id, so the backend can push progress over the same socket. */
  sid?: string | undefined;
  ssl_verify?: boolean | undefined;
  /** Pre-built MCP type (e.g. `mcp_github`) — mutually informative with `url`. */
  toolkit_type?: string | undefined;
  awaitResponse?: boolean;
}

interface McpSyncToolsResult {
  requires_authorization?: boolean;
  response_metadata?: Record<string, unknown>;
  success?: boolean;
  error?: string;
  tools?: readonly unknown[];
  args_schemas?: Record<string, unknown>;
  /**
   * The `approved_only` connector catalogue (ADR-0044 decision 1, #1501):
   * the server is not an approved connector of the project, so `tools` is an
   * unstored preview to request it with.
   */
  connector_approval_required?: boolean;
  connector_target_kind?: string;
  connector_target?: string;
  /** An approved server's tools the approval does not pin (new or changed since); never offered. */
  hidden_tools?: readonly string[];
}

/** Some deployments wrap the result in `{result: {...}}`; others return it at the top level (baseline: `useGetRemoteMcpTools.hooks.js:94`). */
export type McpSyncToolsResponse = McpSyncToolsResult & { result?: McpSyncToolsResult };

export async function mcpSyncTools({ projectId, awaitResponse = true, ...body }: McpSyncToolsParams): Promise<McpSyncToolsResponse> {
  // `eliteaFetch<T>` resolves to the enveloped `{data: T, status, headers}`
  // shape (see `mcpOAuthClient.ts`'s header for the same note).
  const envelope = await eliteaFetch<{ data: McpSyncToolsResponse }>(
    `/elitea_core/mcp_sync_tools/prompt_lib/${projectId}?await_response=${awaitResponse}`,
    {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    },
  );
  return envelope.data;
}
