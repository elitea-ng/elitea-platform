/**
 * The toolkit-authorization card's context and message, ported from the
 * baseline's `features/chat/lib/helpers/mcpAuthorization.helpers.js`
 * (`getToolkitAuthorizationContext`, `buildToolkitAuthorizationMessage`),
 * behaviour unchanged.
 */
import { t } from '@/shared/i18n';

import { normalizeExecutionHierarchy } from './executionHierarchy';

const MCP_AUTHORIZE_PREFIX = 'mcp_authorize_';

type Bag = Readonly<Record<string, unknown>>;

/** The tool-action fields the context is read from — every one optional. */
export interface ToolkitAuthorizationSource {
  readonly name?: unknown;
  readonly toolMeta?: unknown;
  readonly toolOutputs?: unknown;
}

/** What the authorization card shows about the toolkit asking for a login. */
export interface ToolkitAuthorizationContext {
  readonly toolkitName: string;
  readonly toolkitType: string;
  readonly originatorName: string;
  readonly originatorPath: readonly string[];
  readonly serverUrl: string;
  readonly resourceMetadataUrl: string;
  readonly authorizationServers: readonly string[];
  readonly scopes: readonly unknown[];
}

function asBag(value: unknown): Bag {
  return value !== null && typeof value === 'object' ? (value as Bag) : {};
}

/** Truthy string, else `''` — the baseline's `a || b` chains over strings. */
function str(value: unknown): string {
  return typeof value === 'string' ? value : '';
}

const isHttpUrl = (value: string): boolean => /^https?:\/\//i.test(value);

function getAuthorizationServers(metadata: Bag): readonly string[] {
  const fromResource = asBag(metadata['resource_metadata'])['authorization_servers'];
  const servers = fromResource || metadata['authorization_servers'];
  return Array.isArray(servers) ? (servers as string[]) : [];
}

function getToolkitNameFromAction(action: ToolkitAuthorizationSource): string {
  if (typeof action.name !== 'string') return '';
  return action.name.startsWith(MCP_AUTHORIZE_PREFIX) ? action.name.slice(MCP_AUTHORIZE_PREFIX.length) : action.name;
}

function normalizeToolkitType(rawType: string): string {
  const normalizedType = rawType.toLowerCase();
  if (normalizedType.includes('sharepoint')) return 'SharePoint';
  if (normalizedType.includes('openapi') || normalizedType.includes('open_api')) return 'OpenAPI';
  if (normalizedType.includes('mcp')) return 'Remote MCP';
  return rawType;
}

/** Reads the card's context off an MCP/toolkit authorization tool action. */
export function getToolkitAuthorizationContext(action: ToolkitAuthorizationSource): ToolkitAuthorizationContext {
  const metadata = asBag(action.toolMeta);
  const outputs = asBag(action.toolOutputs);
  const resourceMetadata = asBag(metadata['resource_metadata']);
  const resourceName = str(resourceMetadata['resource_name']);
  const rawToolkitName = str(metadata['toolkit_name']) || str(outputs['toolkit_name']);
  const toolkitName =
    (rawToolkitName && !isHttpUrl(rawToolkitName) ? rawToolkitName : '') ||
    getToolkitNameFromAction(action) ||
    str(metadata['toolkit_type']) ||
    str(outputs['toolkit_type']) ||
    resourceName ||
    str(action.name) ||
    'Toolkit';
  const rawType = str(metadata['toolkit_type']) || str(outputs['toolkit_type']) || resourceName;
  const hierarchy = normalizeExecutionHierarchy(action, metadata['metadata'], metadata);
  const path = hierarchy.parent_agent_path;
  const rawScopes = resourceMetadata['scopes_supported'] || metadata['scopes'] || [];
  const scopes: readonly unknown[] = Array.isArray(rawScopes) ? rawScopes : rawScopes ? [rawScopes] : [];

  return {
    toolkitName,
    toolkitType: normalizeToolkitType(rawType),
    originatorName: path[path.length - 1]?.name || hierarchy.parent_agent_name || '',
    originatorPath: path.map((tier) => tier.name).filter(Boolean),
    serverUrl: str(metadata['server_url']),
    resourceMetadataUrl: str(metadata['resource_metadata_url']),
    authorizationServers: getAuthorizationServers(metadata),
    scopes,
  };
}

/** The card's one-line prompt; `fallbackMessage` when there is no context. */
export function buildToolkitAuthorizationMessage(
  context: ToolkitAuthorizationContext | null | undefined,
  fallbackMessage?: string,
): string {
  if (!context) {
    return fallbackMessage ?? t('features.chatMessages.toolkitAuthorization.defaultMessage', 'Toolkit authorization is required.');
  }
  return t(
    'features.chatMessages.toolkitAuthorization.message',
    'Authorization is required for toolkit "{{toolkitName}}". Authorize now or skip it for this run?',
    { toolkitName: context.toolkitName, interpolation: { escapeValue: false } },
  );
}
