/**
 * Port of
 * apps/elitea-ui/src/[fsd]/features/mcp/ui/modal/McpAuthModal.jsx
 * (unit A5, manifest COPY-144). Presents the OAuth-authorization dialog:
 * derives which of client-id/client-secret the user must supply from the
 * auth-server metadata (DCR / OIDC / PKCE / standard flow detection), opens
 * the popup itself (so Cancel can close a still-open one — see
 * `onAuthorize`'s comment), hands it to `startMcpAuthFlow` to drive, and
 * reports success/cancel.
 *
 * DEVIATIONS FROM BASELINE:
 *  - `BaseModal` (`shared/ui`) replaces the hand-rolled MUI `Dialog` markup
 *    — same visual shell unit S1 already established, props-object pattern
 *    (`header`/`actions`) instead of 10 flat props.
 *  - `startMcpAuthFlow` (this slice's own `lib/oauthFlow.ts`) replaces
 *    `McpAuthFlowHelpers.startMcpAuthFlow`.
 */
import type { ReactNode } from 'react';
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';

import Link from '@mui/material/Link';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { BaseModal } from '@/shared/ui/BaseModal';

import {
  applySaveCredentialsPreference,
  buildStartFlowOptions,
  ensureAuthServersAvailable,
  isStringArray,
  pickMcpAuthMetadataFields,
  resolveAuthModalDetailCopy,
  resolveFormDefaults,
  scopesToString,
  tryOpenAuthPopup,
} from '../lib/authModalHelpers';
import { startMcpAuthFlow } from '../lib/oauthFlow';
import { isPrebuildMcpType, getSavedCredentials } from '../lib/storage';
import type { McpAuthMetadata } from '../lib/types';

import { OAuthFormFields } from './OAuthFormFields';

export interface McpAuthModalProps {
  serverUrl?: string | undefined;
  /** Credential-scoped token storage key; falls back to `serverUrl` when absent. */
  tokenStorageKey?: string | undefined;
  mcpAuthMetadata: McpAuthMetadata | null;
  formClientId?: string | undefined;
  formClientSecret?: string | undefined;
  formScopes?: string | readonly string[] | undefined;
  projectId?: string | number | undefined;
  toolkitId?: string | undefined;
  toolkitType?: string | undefined;
  /** Overrides the dialog title (e.g. "Configuration OAuth" for the config-credentials flow). */
  title?: string | undefined;
  open: boolean;
  onClose?: ((success?: boolean) => void) | undefined;
  onCancel?: (() => void) | undefined;
}

export function McpAuthModal(props: McpAuthModalProps): ReactNode {
  const { serverUrl, tokenStorageKey, mcpAuthMetadata, formClientId = '', formClientSecret = '', formScopes, projectId, toolkitId, toolkitType, title, open, onClose, onCancel } = props;

  const storageKey = tokenStorageKey || serverUrl;

  const { authServers, oauthAuthorizationServer, providedSettings, resourceScopes } = pickMcpAuthMetadataFields(mcpAuthMetadata);

  const { clientId: client_id, clientSecret: client_secret, scopes, hasBackendClientId, hasBackendClientSecret } = resolveFormDefaults(
    providedSettings,
    formClientId,
    formClientSecret,
    formScopes,
  );

  const [clientId, setClientId] = useState('');
  const [clientSecret, setClientSecret] = useState('');
  const [scope, setScope] = useState(() => scopesToString(resourceScopes) || scopesToString(scopes));
  const [authLoading, setAuthLoading] = useState(false);
  const [authError, setAuthError] = useState('');
  const [authSuccess, setAuthSuccess] = useState(false);
  const [saveCredentials, setSaveCredentials] = useState(false);
  const authWindowRef = useRef<Window | null>(null);
  const closeTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  const isPrebuildMcp = useMemo(() => isPrebuildMcpType(toolkitType), [toolkitType]);

  useEffect(() => {
    if (!open || (!storageKey && !isPrebuildMcp)) return;
    const savedCreds = getSavedCredentials(storageKey, toolkitType);
    if (savedCreds) {
      setClientId(savedCreds.client_id ?? '');
      setClientSecret(savedCreds.client_secret ?? '');
      setSaveCredentials(true);
    } else {
      setClientId('');
      setClientSecret('');
      setSaveCredentials(false);
    }
    setScope(scopesToString(resourceScopes) || scopesToString(scopes));
    setAuthError('');
    setAuthSuccess(false);
    // `scopes`/`resourceScopes` ARE real dependencies (baseline includes
    // them in this effect's array too): if the modal is re-targeted at a
    // different resource's auth metadata (a new `mcpAuthMetadata` prop)
    // while it stays open — same `storageKey`/`toolkitType` — the scope
    // input must re-derive from the new resource's scopes, not keep
    // showing the previous one's.
  }, [open, storageKey, isPrebuildMcp, toolkitType, scopes, resourceScopes]);

  useEffect(() => {
    return () => {
      if (closeTimerRef.current) clearTimeout(closeTimerRef.current);
    };
  }, []);

  const availableScopes = useMemo((): readonly string[] => {
    if (resourceScopes && resourceScopes.length > 0) return resourceScopes;
    if (oauthAuthorizationServer?.scopes_supported && oauthAuthorizationServer.scopes_supported.length > 0) return oauthAuthorizationServer.scopes_supported;
    return isStringArray(scopes) ? scopes : [];
  }, [resourceScopes, oauthAuthorizationServer, scopes]);

  const serverMetadata = useMemo(() => {
    const metadata = oauthAuthorizationServer ?? {};
    const isActuallyOIDC = Boolean(metadata.userinfo_endpoint);
    const authMethods = metadata.token_endpoint_auth_methods_supported ?? [];
    const supportsPKCE = metadata.code_challenge_methods_supported?.includes('S256') ?? false;
    const supportsPublicClients = authMethods.length === 0 || authMethods.includes('none') || supportsPKCE;
    const requiresClientSecret = authMethods.length > 0 && !authMethods.includes('none') && !supportsPKCE;
    const hasDCREndpoint = Boolean(metadata.registration_endpoint);
    const canUseDCR = hasDCREndpoint && supportsPublicClients;

    return {
      supportsPKCE,
      supportsDCR: canUseDCR,
      requiresClientSecret,
      isOIDC: isActuallyOIDC,
    };
  }, [oauthAuthorizationServer]);

  const authFlow = useMemo(() => {
    if (serverMetadata.supportsDCR) return 'dcr';
    if (serverMetadata.isOIDC) return 'oidc';
    if (serverMetadata.supportsPKCE) return 'pkce';
    return 'standard';
  }, [serverMetadata.supportsDCR, serverMetadata.isOIDC, serverMetadata.supportsPKCE]);

  const needClientId = useMemo(() => {
    if (hasBackendClientId) return false;
    if (authFlow === 'dcr') return false;
    return !client_id.trim();
  }, [authFlow, client_id, hasBackendClientId]);

  const needsClientSecret = useMemo(() => {
    if (hasBackendClientSecret) return false;
    if (serverMetadata.requiresClientSecret) return !client_secret.trim();
    if (authFlow === 'oidc' || authFlow === 'dcr') return false;
    if (authFlow === 'pkce' && serverMetadata.supportsPKCE) return false;
    return !client_secret.trim();
  }, [authFlow, client_secret, hasBackendClientSecret, serverMetadata.supportsPKCE, serverMetadata.requiresClientSecret]);

  // Grouped derivations (§3.5 hook-deps budget: max 8 per array) —
  // each combines fields that already change together (both from the
  // `mcpAuthMetadata`/loading-lifecycle/flow-identity source).
  const hasMetadata = useMemo(
    () => Boolean(oauthAuthorizationServer) || Boolean(authServers && authServers.length > 0),
    [oauthAuthorizationServer, authServers],
  );
  const isAuthLifecycleActive = authLoading || authSuccess;
  const credentials = useMemo(
    () => ({ clientId: client_id.trim() || clientId, clientSecret: client_secret.trim() || clientSecret }),
    [client_id, client_secret, clientId, clientSecret],
  );
  const flowContext = useMemo(() => ({ toolkitId, toolkitType, projectId, serverUrl }), [toolkitId, toolkitType, projectId, serverUrl]);

  const isAuthorizeDisabled = useMemo(() => {
    if (isAuthLifecycleActive) return true;
    if (!storageKey && !isPrebuildMcp) return true;
    if (!hasMetadata) return true;
    if (needClientId && !clientId.trim()) return true;
    return needsClientSecret && !clientSecret.trim();
  }, [isAuthLifecycleActive, storageKey, isPrebuildMcp, needClientId, needsClientSecret, clientId, clientSecret, hasMetadata]);

  const handleCancel = useCallback(() => {
    if (authWindowRef.current && !authWindowRef.current.closed) authWindowRef.current.close();
    authWindowRef.current = null;
    setAuthLoading(false);
    setAuthError('');
    setAuthSuccess(false);
    onCancel?.();
  }, [onCancel]);

  const onAuthorize = useCallback(async () => {
    if (!storageKey && !isPrebuildMcp) return;

    // Opened HERE (synchronously, inside the click handler, before any
    // await) rather than letting `startMcpAuthFlow` open it internally —
    // this modal needs its own reference so Cancel can close a still-open
    // popup (baseline: `onAuthorize`'s `authWindowRef.current.close()`).
    // `startMcpAuthFlow` accepts a pre-opened `authWindow` for exactly
    // this reason (`oauthFlow.ts`'s own doc comment).
    const popupResult = tryOpenAuthPopup();
    if ('error' in popupResult) {
      setAuthError(popupResult.error);
      return;
    }
    const { authWindow } = popupResult;
    authWindowRef.current = authWindow;

    setAuthLoading(true);
    setAuthError('');
    setAuthSuccess(false);
    try {
      const validated = ensureAuthServersAvailable(mcpAuthMetadata);
      const startFlowOptions = buildStartFlowOptions({ storageKey, validated, authWindow, credentials, scope, flowContext, isPrebuildMcp });
      await startMcpAuthFlow(startFlowOptions);

      applySaveCredentialsPreference({ saveCredentials, storageKey, credentials, toolkitType: flowContext.toolkitType });

      authWindowRef.current = null;
      setAuthSuccess(true);
      closeTimerRef.current = setTimeout(() => onClose?.(true), 1500);
    } catch (error) {
      setAuthError(error instanceof Error ? error.message : 'Authorization failed');
      authWindowRef.current = null;
    } finally {
      setAuthLoading(false);
    }
  }, [storageKey, mcpAuthMetadata, credentials, scope, onClose, flowContext, isPrebuildMcp, saveCredentials]);

  const handleKeyDown = useCallback(
    (event: { key: string; preventDefault: () => void }) => {
      if (event.key === 'Enter' && !isAuthorizeDisabled) {
        event.preventDefault();
        void onAuthorize();
      } else if (event.key === 'Escape') {
        event.preventDefault();
        handleCancel();
      }
    },
    [isAuthorizeDisabled, onAuthorize, handleCancel],
  );

  return (
    <BaseModal
      open={open}
      onClose={handleCancel}
      onKeyDown={handleKeyDown}
      title={title ?? t('mcps.authModal.title', 'MCP Authorization')}
      data-testid="mcp-auth-modal"
      content={
        <>
          <Typography
            variant="bodyMedium"
            component="div"
            sx={{ marginBottom: '1rem' }}
          >
            {t('mcps.authModal.description', 'This MCP server requires OAuth authorization to access its tools. {{detail}}', {
              detail: resolveAuthModalDetailCopy(serverMetadata.requiresClientSecret, authFlow),
            })}
          </Typography>
          <Typography
            variant="headingSmall"
            component="div"
            sx={{ color: 'text.secondary' }}
          >
            {t('mcps.authModal.serverLabel', 'Server: ')}
            <Typography
              variant="bodyMedium"
              component="span"
            >
              <Link
                href={serverUrl}
                target="_blank"
                rel="noopener noreferrer"
              >
                {serverUrl}
              </Link>
            </Typography>
          </Typography>
          <OAuthFormFields
            clientId={clientId}
            clientSecret={clientSecret}
            scope={scope}
            onClientIdChange={setClientId}
            onClientSecretChange={setClientSecret}
            onScopeChange={setScope}
            availableScopes={availableScopes}
            needSecret={needsClientSecret}
            needClientId={needClientId}
            saveCredentials={saveCredentials}
            onSaveCredentialsChange={setSaveCredentials}
            showSaveCredentials={needClientId || needsClientSecret}
          />
          {authError && (
            <Typography
              component="div"
              variant="bodyMedium"
              sx={{ color: 'status.rejected', marginTop: '1rem' }}
            >
              {authError}
            </Typography>
          )}
          {authSuccess && (
            <Typography
              variant="bodyMedium"
              component="div"
              sx={{ color: 'status.published', marginTop: '1rem' }}
            >
              {t('mcps.authModal.success', 'Authorization successful! Your credentials and session have been saved. Please send your message again to use the authorized MCP server.')}
            </Typography>
          )}
        </>
      }
      actions={{
        cancelText: t('mcps.authModal.cancel', 'Cancel'),
        confirmText: authLoading ? t('mcps.authModal.authorizing', 'Authorizing…') : t('mcps.authModal.authorize', 'Authorize'),
        // BaseModal's action bar only exposes a `confirming` disable
        // switch (no separate raw `disabled`) — reused here to cover the
        // baseline's full `isAuthorizeDisabled` condition (missing
        // metadata/credentials, in-flight, or already succeeded), not just
        // "request in flight", since the confirm button must always be
        // VISIBLE (baseline: `disabled={isAuthorizeDisabled}`, never
        // unmounted) while still being unclickable in those states.
        confirming: isAuthorizeDisabled,
      }}
      onConfirm={() => void onAuthorize()}
    />
  );
}
