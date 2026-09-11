/** Build durable delegated-authorization cards from live or persisted metadata. */
import { TOOL_ACTION_TYPES, ToolActionStatus } from '@/shared/lib/chat';

import type { ToolActionDraft } from './toolActions';

const PRIVATE_KEYS = new Set([
  'accesstoken',
  'refreshtoken',
  'idtoken',
  'clientsecret',
  'mcpclientsecret',
  'apikey',
  'xapikey',
  'authorization',
  'proxyauthorization',
  'cookie',
  'setcookie',
]);

function privateKey(key: string): boolean {
  return PRIVATE_KEYS.has(key.toLowerCase().replaceAll('_', '').replaceAll('-', ''));
}

function record(value: unknown): Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : {};
}

function sanitized(value: unknown, depth = 0): unknown {
  if (depth > 6) return undefined;
  if (Array.isArray(value)) return value.map((item) => sanitized(item, depth + 1));
  if (typeof value !== 'object' || value === null) return value;
  const result: Record<string, unknown> = {};
  for (const [key, child] of Object.entries(value)) {
    // This constant is a backend credential reference, never a secret value.
    if (key === 'mcp_client_secret' && child === '********') result[key] = child;
    else if (!privateKey(key)) result[key] = sanitized(child, depth + 1);
  }
  return result;
}

function stringValue(source: Record<string, unknown>, key: string): string | undefined {
  const value = source[key];
  return typeof value === 'string' && value !== '' ? value : undefined;
}

function stringList(value: unknown): readonly string[] {
  return Array.isArray(value)
    ? value.filter((item): item is string => typeof item === 'string' && item !== '')
    : [];
}

function requestId(source: Record<string, unknown>): string | undefined {
  return stringValue(source, 'authorization_request_id')
    ?? stringValue(source, 'interrupt_id')
    ?? stringValue(source, 'tool_run_id');
}

function authorizationServers(source: Record<string, unknown>): readonly string[] {
  const resource = record(source['resource_metadata']);
  return stringList(resource['authorization_servers']).length > 0
    ? stringList(resource['authorization_servers'])
    : stringList(source['authorization_servers']);
}

function tokenStorageKey(source: Record<string, unknown>, serverUrl: string): string {
  const resource = record(source['resource_metadata']);
  const oauthEndpoint = authorizationServers(source)[0];
  const configurationUuid = stringValue(resource, 'configuration_uuid');
  const toolkitType = stringValue(source, 'toolkit_type');
  const resourceName = stringValue(resource, 'resource_name');
  if (toolkitType?.startsWith('mcp_') && toolkitType !== 'mcp') return toolkitType;
  if (configurationUuid && oauthEndpoint) return `${configurationUuid}:${oauthEndpoint}`;
  if ((resourceName === 'SharePoint' || resourceName === 'OpenAPI') && oauthEndpoint) return oauthEndpoint;
  return serverUrl;
}

function authorizationSources(metadata: Record<string, unknown>): readonly Record<string, unknown>[] {
  const pending = metadata['authorization_requests'];
  if (Array.isArray(pending) && pending.length > 0) return pending.map(record);
  return metadata['guardrail_type'] === 'mcp_auth' || requestId(metadata) ? [metadata] : [];
}

function authorizationContent(
  source: Record<string, unknown>,
  servers: readonly string[],
  fallbackContent: string,
): string {
  if (servers.length === 0) {
    if (hasDurableSkip(source)) return 'Authorization details are unavailable. The tool has not run.';
    const status = typeof source['status'] === 'number' ? source['status'] : 401;
    const toolName = stringValue(source, 'tool_name') ?? 'MCP toolkit';
    const serverUrl = stringValue(source, 'server_url') ?? 'MCP server';
    return `${status}: Authorization error in "${toolName}" toolkit.\n\n` +
      `The toolkit server at ${serverUrl} requires OAuth authorization, but did not provide authorization server configuration.`;
  }
  const message = (stringValue(source, 'message') ?? fallbackContent) || 'Authorization required.';
  const metadataUrl = stringValue(source, 'resource_metadata_url');
  const discovery = metadataUrl
    ? `Resource metadata: ${metadataUrl}`
    : `Authorization servers: ${servers.join(', ')}`;
  return `${message}\n\n${discovery}`;
}

function hasDurableSkip(source: Record<string, unknown>): boolean {
  return source['guardrail_type'] === 'mcp_auth'
    && Boolean(stringValue(source, 'interrupt_id'))
    && stringList(source['available_actions']).includes('skip');
}

function authorizationHierarchy(source: Record<string, unknown>) {
  const nested = record(source['metadata']);
  return {
    parent_agent_path: source['parent_agent_path'] ?? nested['parent_agent_path'] ?? [],
    parent_agent_name: stringValue(source, 'parent_agent_name') ?? stringValue(nested, 'parent_agent_name'),
    parent_agent_call_id: stringValue(source, 'parent_agent_call_id') ?? stringValue(nested, 'parent_agent_call_id'),
  };
}

function buildAuthorizationAction(
  source: Record<string, unknown>,
  fallbackContent: string,
  createdAt: string,
): ToolActionDraft | undefined {
  const exactId = requestId(source);
  if (!exactId) return undefined;
  const safeMetadata = sanitized(source) as Record<string, unknown>;
  delete safeMetadata['authorization_requests'];
  const servers = authorizationServers(safeMetadata);
  const toolName = stringValue(safeMetadata, 'tool_name') ?? 'MCP toolkit';
  const serverUrl = stringValue(safeMetadata, 'server_url') ?? servers[0] ?? 'MCP server';
  const canAuthorize = servers.length > 0;
  // Discovery failure must not hide a checkpoint-owned Skip decision.
  const isPending = canAuthorize || hasDurableSkip(safeMetadata);
  return {
    id: exactId,
    authorizationRequestId: exactId,
    name: toolName,
    status: isPending ? ToolActionStatus.actionRequired : ToolActionStatus.error,
    type: TOOL_ACTION_TYPES.Toolkit,
    toolInputs: undefined,
    toolOutputs: isPending
      ? {
          resource_metadata_url: safeMetadata['resource_metadata_url'] ?? null,
          authorization_servers: servers,
          server_url: tokenStorageKey(safeMetadata, serverUrl),
        }
      : undefined,
    toolMeta: safeMetadata,
    response_metadata: safeMetadata,
    ...authorizationHierarchy(safeMetadata),
    created_at: createdAt,
    ended_at: createdAt,
    timestamp: createdAt,
    markdown: false,
    renderHtml: false,
    isError: !isPending,
    content: authorizationContent(safeMetadata, servers, fallbackContent),
  };
}

/** Return one unique card for each exact authorization interrupt. */
export function buildAuthorizationActions(
  metadata: Record<string, unknown>,
  fallbackContent: string,
  createdAt: string,
): readonly ToolActionDraft[] {
  const seen = new Set<string>();
  const actions: ToolActionDraft[] = [];
  for (const source of authorizationSources(metadata)) {
    const action = buildAuthorizationAction(source, fallbackContent, createdAt);
    if (!action?.id || seen.has(action.id)) continue;
    seen.add(action.id);
    actions.push(action);
  }
  return actions;
}
