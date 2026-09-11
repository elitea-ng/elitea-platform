/**
 * Pure OAuth-metadata extraction/normalisation helpers — port of the
 * metadata half of
 * apps/elitea-ui/src/[fsd]/features/mcp/lib/helpers/mcpDiscovery.helpers.js
 * AND
 * apps/elitea-ui/src/[fsd]/features/mcp/lib/hooks/useMcpAuthModal.hooks.js's
 * two exported extractors (`extractMcpAuthMetadata`/`extractConfigAuthMetadata`
 * are pure functions, not hooks — moved here so `model/useMcpAuthModal.ts`
 * only holds actual React state). `registerDynamicClient` (the
 * network-calling half of `mcpDiscovery.helpers.js`) lives in
 * `api/mcpOAuthClient.ts` (API-166).
 *
 * No network, no storage, no `window` — everything here is a pure function
 * over plain objects, which is what makes it the highest-confidence part of
 * this slice to test exhaustively.
 */
import { MCP_OAUTH_ERRORS } from './constants';
import type { McpAuthMetadata, McpProvidedSettings, OAuthServerMetadata } from './types';

/**
 * Loosely-typed source: either a streamed socket message or a
 * `toolActions`-style auth-required action — both shapes flow into this
 * extractor in the baseline. Every field is `T | undefined` (not bare
 * `?: T`) — `exactOptionalPropertyTypes`, same reasoning as `types.ts`'s
 * header: callers (this module's own extractors, plus `useMcpAuthModal.ts`'s
 * `AuthRequiredMessage` extension) construct/narrow these from other
 * optional sources.
 */
export interface McpAuthMetadataSource {
  response_metadata?:
    | {
        resource_metadata?: RawResourceMetadata | undefined;
        provided_settings?: McpProvidedSettings | undefined;
        authorization_servers?: readonly string[] | undefined;
        toolkit_id?: string | undefined;
      }
    | undefined;
  toolMeta?:
    | {
        resource_metadata?: RawResourceMetadata | undefined;
        provided_settings?: McpProvidedSettings | undefined;
        toolkit_id?: string | undefined;
      }
    | undefined;
  toolOutputs?:
    | {
        authorization_servers?: readonly string[] | undefined;
      }
    | undefined;
}

interface RawResourceMetadata {
  oauth_authorization_server?: OAuthServerMetadata | undefined;
  authorization_servers?: readonly string[] | undefined;
  provided_settings?: McpProvidedSettings | undefined;
  scopes_supported?: readonly string[] | undefined;
  configuration_uuid?: string | undefined;
  toolkit_id?: string | undefined;
}

function toOAuthMetadata(server: OAuthServerMetadata | undefined): OAuthServerMetadata | null {
  if (!server) return null;
  return {
    token_endpoint: server.token_endpoint,
    authorization_endpoint: server.authorization_endpoint,
    revocation_endpoint: server.revocation_endpoint,
    registration_endpoint: server.registration_endpoint,
    issuer: server.issuer,
    grant_types_supported: server.grant_types_supported,
    code_challenge_methods_supported: server.code_challenge_methods_supported,
    token_endpoint_auth_methods_supported: server.token_endpoint_auth_methods_supported,
    scopes_supported: server.scopes_supported,
  };
}

type ResponseMetadataPart = NonNullable<McpAuthMetadataSource['response_metadata']>;
type ToolMetaPart = NonNullable<McpAuthMetadataSource['toolMeta']>;

/** One `??` fallback chain per field, split out of `extractMcpAuthMetadata` (§3.5 complexity budget: the inlined form measured 16). */
function pickMcpResourceMetadata(responseMetadata: ResponseMetadataPart, toolMeta: ToolMetaPart): RawResourceMetadata {
  return responseMetadata.resource_metadata ?? toolMeta.resource_metadata ?? {};
}

function pickMcpProvidedSettings(responseMetadata: ResponseMetadataPart, toolMeta: ToolMetaPart, resourceMetadata: RawResourceMetadata): McpProvidedSettings {
  return responseMetadata.provided_settings ?? toolMeta.provided_settings ?? resourceMetadata.provided_settings ?? {};
}

function pickMcpAuthServers(
  resourceMetadata: RawResourceMetadata,
  responseMetadata: ResponseMetadataPart,
  toolOutputs: NonNullable<McpAuthMetadataSource['toolOutputs']>,
): readonly string[] | undefined {
  return resourceMetadata.authorization_servers ?? responseMetadata.authorization_servers ?? toolOutputs.authorization_servers;
}

function pickMcpToolkitId(responseMetadata: ResponseMetadataPart, toolMeta: ToolMetaPart, resourceMetadata: RawResourceMetadata): string | undefined {
  return responseMetadata.toolkit_id ?? toolMeta.toolkit_id ?? resourceMetadata.toolkit_id;
}

/**
 * Extracts MCP auth metadata from a streamed `mcp_authorization_required`
 * message OR a `toolActions`-style `authRequiredAction`. Both shapes are
 * supported because the baseline's `useGetRemoteMcpTools` and chat's tool
 * actions each hand this a differently-nested object.
 */
export function extractMcpAuthMetadata(source: McpAuthMetadataSource | null | undefined): McpAuthMetadata {
  const responseMetadata = source?.response_metadata ?? {};
  const toolMeta = source?.toolMeta ?? {};
  const toolOutputs = source?.toolOutputs ?? {};

  const resourceMetadata = pickMcpResourceMetadata(responseMetadata, toolMeta);
  const oauthServer = resourceMetadata.oauth_authorization_server;

  return {
    authServers: pickMcpAuthServers(resourceMetadata, responseMetadata, toolOutputs),
    oauthAuthorizationServer: oauthServer,
    oauthMetadata: toOAuthMetadata(oauthServer),
    providedSettings: pickMcpProvidedSettings(responseMetadata, toolMeta, resourceMetadata),
    resourceScopes: resourceMetadata.scopes_supported,
    configurationUuid: resourceMetadata.configuration_uuid,
    toolkitId: pickMcpToolkitId(responseMetadata, toolMeta, resourceMetadata),
  };
}

/** `check_connection` 401 response shape -> the same `McpAuthMetadata` `McpAuthModal` consumes. */
export interface ConfigAuthMetadataSource {
  resource_metadata?: RawResourceMetadata | undefined;
  authorization_servers?: readonly string[] | undefined;
}

export function extractConfigAuthMetadata(authMetadata: ConfigAuthMetadataSource | null | undefined): McpAuthMetadata | null {
  if (!authMetadata) return null;
  const resourceMetadata = authMetadata.resource_metadata ?? {};
  const oauthServer = resourceMetadata.oauth_authorization_server;
  return {
    authServers: resourceMetadata.authorization_servers ?? authMetadata.authorization_servers ?? [],
    oauthAuthorizationServer: oauthServer,
    oauthMetadata: toOAuthMetadata(oauthServer),
    providedSettings: resourceMetadata.provided_settings ?? {},
    resourceScopes: resourceMetadata.scopes_supported,
  };
}

/**
 * Common OAuth endpoint conventions for authorization servers that don't
 * expose an OIDC/OAuth discovery document (e.g. GitHub's `/authorize` +
 * `/access_token`), used ONLY as a last-resort fallback.
 */
function constructOAuthMetadataFromServer(authServerUrl: string | undefined): Pick<OAuthServerMetadata, 'authorization_endpoint' | 'token_endpoint'> | null {
  if (!authServerUrl) return null;
  const normalizedUrl = authServerUrl.replace(/\/+$/, '');
  return {
    authorization_endpoint: `${normalizedUrl}/authorize`,
    token_endpoint: `${normalizedUrl}/access_token`,
  };
}

interface AuthServerMetadataSource {
  oauth_authorization_server?: OAuthServerMetadata | undefined;
  authorization_server?: OAuthServerMetadata | undefined;
  authorization_endpoint?: string | undefined;
  token_endpoint?: string | undefined;
  authorization_servers?: readonly string[] | undefined;
  /** Mirrors `OAuthServerMetadata`'s own index signature — `resolveDirectOrNestedAuthServerMetadata` returns this object AS an `OAuthServerMetadata` when it directly carries both endpoints, so the two types must stay structurally compatible. */
  [key: string]: unknown;
}

/** Nested `oauth_authorization_server`/`authorization_server`, or the source object itself when it directly carries both endpoints. */
function resolveDirectOrNestedAuthServerMetadata(metadata: AuthServerMetadataSource | null | undefined): OAuthServerMetadata | undefined {
  const nested = metadata?.oauth_authorization_server ?? metadata?.authorization_server;
  if (nested) return nested;
  if (metadata?.authorization_endpoint && metadata.token_endpoint) return metadata;
  return undefined;
}

function hasUsableAuthEndpoints(metadata: OAuthServerMetadata | undefined): boolean {
  return Boolean(metadata?.authorization_endpoint && metadata?.token_endpoint);
}

/** Last-resort fallback built from `authorization_servers[0]`'s host, per `constructOAuthMetadataFromServer`'s convention-based endpoints. */
function resolveFallbackAuthServerMetadata(metadata: AuthServerMetadataSource | null | undefined): Pick<OAuthServerMetadata, 'authorization_endpoint' | 'token_endpoint'> | undefined {
  const authServers = metadata?.authorization_servers;
  if (!authServers || authServers.length === 0) return undefined;
  return constructOAuthMetadataFromServer(authServers[0]) ?? undefined;
}

/**
 * Resolves the auth-server metadata `startMcpAuthFlow` needs from whatever
 * the `mcp_authorization_required` message provided — never performs its
 * own discovery fetch (that's an explicit baseline design choice: metadata
 * must come from the backend). Throws `MCP_OAUTH_ERRORS.NO_AUTH_SERVERS`/
 * `MISSING_ENDPOINTS` when nothing usable is present.
 *
 * Split into `resolveDirectOrNested`/`hasUsable`/`resolveFallback` helpers
 * (§3.5 complexity budget: the single-function form measured 20).
 */
export function extractAuthServerMetadata(metadata: AuthServerMetadataSource | null | undefined): OAuthServerMetadata {
  let asMetadata = resolveDirectOrNestedAuthServerMetadata(metadata);

  if (!hasUsableAuthEndpoints(asMetadata)) {
    const fallback = resolveFallbackAuthServerMetadata(metadata);
    if (fallback) {
      asMetadata = { ...asMetadata, ...fallback };
    }
  }

  if (!asMetadata) {
    throw new Error(MCP_OAUTH_ERRORS.NO_AUTH_SERVERS);
  }
  if (!asMetadata.authorization_endpoint || !asMetadata.token_endpoint) {
    throw new Error(MCP_OAUTH_ERRORS.MISSING_ENDPOINTS);
  }

  return asMetadata;
}
