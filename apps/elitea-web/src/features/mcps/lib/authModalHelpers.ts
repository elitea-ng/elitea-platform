/**
 * Pure/near-pure helpers for `ui/McpAuthModal.tsx` — split into `lib/` for
 * two reasons: (1) §3.5's 400-line file budget (the component plus these
 * helpers inline measured 468 lines), and (2) these are plain module-scope
 * functions, a genuinely separate scope for the `complexity` rule, so
 * extracting them is also what brought the component's own measured
 * complexity down from 26 (and `onAuthorize`'s inlined body down from 17) —
 * see each function's own comment for the specific budget it addresses.
 */
import { t } from '@/shared/i18n';

import type { StartMcpAuthFlowOptions } from './oauthFlow';
import { removeSavedCredentials, setSavedCredentials } from './storage';
import type { McpAuthMetadata, McpProvidedSettings, OAuthServerMetadata } from './types';
import { openAuthPopup } from './window';

/** TS's built-in `Array.isArray` type guard narrows to `any[]` (a `lib.es5.d.ts` imprecision, not this project's doing) — this local guard narrows to the concrete `readonly string[]` the modal actually needs. */
export function isStringArray(value: unknown): value is readonly string[] {
  return Array.isArray(value);
}

export function scopesToString(scopes: string | readonly string[] | undefined): string {
  if (Array.isArray(scopes)) return scopes.join(' ').trim();
  if (typeof scopes === 'string') return scopes;
  return '';
}

interface McpAuthMetadataFields {
  authServers: readonly string[] | undefined;
  oauthAuthorizationServer: OAuthServerMetadata | undefined;
  providedSettings: McpProvidedSettings | undefined;
  resourceScopes: readonly string[] | undefined;
}

/** The four `mcpAuthMetadata?.x` reads the component needs, in one place. */
export function pickMcpAuthMetadataFields(mcpAuthMetadata: McpAuthMetadata | null): McpAuthMetadataFields {
  return {
    authServers: mcpAuthMetadata?.authServers,
    oauthAuthorizationServer: mcpAuthMetadata?.oauthAuthorizationServer,
    providedSettings: mcpAuthMetadata?.providedSettings,
    resourceScopes: mcpAuthMetadata?.resourceScopes,
  };
}

interface ResolvedFormDefaults {
  clientId: string;
  clientSecret: string;
  scopes: string | readonly string[] | undefined;
  hasBackendClientId: boolean;
  hasBackendClientSecret: boolean;
}

/** Backend-provided (`provided_settings`) values win over the caller's form defaults, matching the baseline's `||` fallback chain. */
export function resolveFormDefaults(
  providedSettings: McpProvidedSettings | undefined,
  formClientId: string,
  formClientSecret: string,
  formScopes: string | readonly string[] | undefined,
): ResolvedFormDefaults {
  return {
    clientId: providedSettings?.mcp_client_id || formClientId,
    clientSecret: providedSettings?.mcp_client_secret || formClientSecret,
    scopes: providedSettings?.scopes || formScopes,
    hasBackendClientId: Boolean(providedSettings?.mcp_client_id),
    hasBackendClientSecret: Boolean(providedSettings?.mcp_client_secret),
  };
}

export type McpAuthFlowKind = 'dcr' | 'oidc' | 'pkce' | 'standard';

/** The copy explaining what kind of flow this server uses, shown under the modal's main description. */
export function resolveAuthModalDetailCopy(requiresClientSecret: boolean, authFlow: McpAuthFlowKind): string {
  if (requiresClientSecret) {
    return t('mcps.authModal.detailPreRegistered', 'This server requires a pre-registered OAuth application. Please provide your client credentials.');
  }
  if (authFlow === 'oidc') return t('mcps.authModal.detailOidc', 'Using OIDC flow.');
  if (authFlow === 'dcr') return t('mcps.authModal.detailDcr', 'Supports automatic client registration.');
  if (authFlow === 'pkce') return t('mcps.authModal.detailPkce', 'Using PKCE flow for enhanced security.');
  return '';
}

export type AuthPopupResult = { authWindow: Window } | { error: string };

/** Opens the popup synchronously (so Cancel can close it) — see `onAuthorize`'s own comment for why this can't be `startMcpAuthFlow`'s job. */
export function tryOpenAuthPopup(): AuthPopupResult {
  const authWindow = openAuthPopup();
  if (!authWindow) {
    return { error: 'Popup blocked. Please allow popups for this site and try again.' };
  }
  return { authWindow };
}

export interface ValidatedAuthServers {
  authServers: readonly string[];
  oauthAuthorizationServer: OAuthServerMetadata | undefined;
  oauthMetadata: Partial<OAuthServerMetadata> | null | undefined;
}

/** Throws when the metadata this modal was opened with carries no usable authorization servers — an invariant violation, not a user-facing validation state. */
export function ensureAuthServersAvailable(mcpAuthMetadata: McpAuthMetadata | null): ValidatedAuthServers {
  if (!mcpAuthMetadata?.authServers || mcpAuthMetadata.authServers.length === 0) {
    throw new Error('No authorization servers available');
  }
  return {
    authServers: mcpAuthMetadata.authServers,
    oauthAuthorizationServer: mcpAuthMetadata.oauthAuthorizationServer,
    oauthMetadata: mcpAuthMetadata.oauthMetadata,
  };
}

export interface BuildStartFlowOptionsParams {
  storageKey: string | undefined;
  validated: ValidatedAuthServers;
  authWindow: Window;
  credentials: { clientId: string; clientSecret: string };
  scope: string;
  flowContext: { toolkitId: string | undefined; toolkitType: string | undefined; projectId: string | number | undefined; serverUrl?: string | undefined };
  isPrebuildMcp: boolean;
}

export function buildStartFlowOptions(params: BuildStartFlowOptionsParams): StartMcpAuthFlowOptions {
  const { validated, flowContext, isPrebuildMcp } = params;
  const asForFlow = validated.oauthAuthorizationServer;
  return {
    serverUrl: params.storageKey,
    resourceUrl: flowContext.toolkitType === 'mcp' || isPrebuildMcp ? flowContext.serverUrl : undefined,
    resourceMetadata: { authorization_servers: validated.authServers, oauth_authorization_server: asForFlow },
    oauthMetadata: validated.oauthMetadata ?? { token_endpoint: asForFlow?.token_endpoint, grant_types_supported: asForFlow?.grant_types_supported },
    clientId: params.credentials.clientId,
    clientSecret: params.credentials.clientSecret,
    scope: params.scope,
    authWindow: params.authWindow,
    projectId: flowContext.projectId,
    toolkitId: flowContext.toolkitId,
    toolkitType: isPrebuildMcp ? flowContext.toolkitType : undefined,
  };
}

export interface ApplySaveCredentialsPreferenceParams {
  saveCredentials: boolean;
  storageKey: string | undefined;
  credentials: { clientId: string; clientSecret: string };
  toolkitType: string | undefined;
}

/** Persists the entered credentials when the user opted in and there's something to save; clears any previously-saved ones otherwise. */
export function applySaveCredentialsPreference(params: ApplySaveCredentialsPreferenceParams): void {
  const { saveCredentials, storageKey, credentials, toolkitType } = params;
  if (saveCredentials && (credentials.clientId || credentials.clientSecret)) {
    setSavedCredentials({ serverUrl: storageKey, clientId: credentials.clientId, clientSecret: credentials.clientSecret, toolkitType });
    return;
  }
  if (!saveCredentials) {
    removeSavedCredentials(storageKey, toolkitType);
  }
}
