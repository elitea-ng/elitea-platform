/**
 * Port of
 * apps/elitea-ui/src/[fsd]/features/mcp/lib/hooks/useMcpAuthModal.hooks.js
 * (unit A5) — modal-open/close STATE only; the pure metadata extractors the
 * baseline co-located in this file now live in `../lib/discoveryMetadata.ts`.
 *
 * DEVIATION FROM BASELINE (toast): the baseline calls `useToast().toastSuccess(...)`
 * directly on a successful close. No toast/snackbar primitive exists yet in
 * `shared/ui` (S1's 67-component landing did not include one) — out of this
 * unit's ownership fence to build. `handleCloseModal`/`getModalProps().onClose`
 * still receive a `success: boolean` flag; the caller decides how to
 * surface it (inline text, or a real toast once one exists). The
 * `showSuccessToast` option baseline exposed is dropped entirely rather
 * than kept as a dead no-op flag.
 *
 * DEVIATION FROM BASELINE (projectId): `projectId` is an explicit option instead of an
 * internal `useSelectedProjectId()` call. The old app reads it from Redux;
 * this app has no reactive "current project" accessor `features/` may
 * import (`src/app/router-context.ts`'s `AuthContext.getSelectedProjectId()`
 * is `beforeLoad`-only/non-reactive, and `widgets/app-shell`'s project
 * store is a LAYER ABOVE `features/` — R-L1 forbids the import either way).
 * The caller (a `pages/`/`widgets/` component, which sits above `features/`
 * and DOES have a legitimate project-id source) passes it down — see the
 * A5 final report for the full reasoning.
 */
import { useCallback, useMemo, useState } from 'react';

import type { McpAuthMetadataSource, ConfigAuthMetadataSource } from '../lib/discoveryMetadata';
import { extractConfigAuthMetadata, extractMcpAuthMetadata } from '../lib/discoveryMetadata';
import { isPrebuildMcpType } from '../lib/storage';
import type { McpAuthMetadata } from '../lib/types';

export interface McpAuthModalValues {
  id?: string | undefined;
  type?: string | undefined;
  settings?: { url?: string | undefined; client_id?: string | undefined; client_secret?: string | undefined; scopes?: string | readonly string[] | undefined } | undefined;
}

export interface UseMcpAuthModalOptions {
  onSuccess?: (() => void) | undefined;
  values?: McpAuthModalValues | undefined;
  projectId?: string | number | undefined;
}

export interface McpAuthModalRenderProps {
  open: boolean;
  serverUrl: string | undefined;
  tokenStorageKey: string | undefined;
  mcpAuthMetadata: McpAuthMetadata | null;
  formClientId: string | undefined;
  formClientSecret: string | undefined;
  formScopes: string | readonly string[] | undefined;
  projectId: string | number | undefined;
  toolkitId: string | undefined;
  toolkitType: string | undefined;
  onClose: (success?: boolean) => void;
  onCancel: () => void;
}

export interface UseMcpAuthModalResult {
  showModal: boolean;
  mcpAuthMetadata: McpAuthMetadata | null;
  runtimeServerUrl: string;
  handleMcpAuthRequired: (message: AuthRequiredMessage | null | undefined) => void;
  handleCloseModal: (success?: boolean) => void;
  handleCancelModal: () => void;
  openModal: (metadata: McpAuthMetadata) => void;
  getModalProps: () => McpAuthModalRenderProps;
}

/** Extra fields `mcp_authorization_required` messages carry alongside the metadata shape (`server_url`). */
interface AuthRequiredMessage extends McpAuthMetadataSource {
  response_metadata?: (McpAuthMetadataSource['response_metadata'] & { server_url?: string }) | undefined;
}

export function useMcpAuthModal(options: UseMcpAuthModalOptions = {}): UseMcpAuthModalResult {
  const { onSuccess, values, projectId } = options;

  const [showModal, setShowModal] = useState(false);
  const [mcpAuthMetadata, setMcpAuthMetadata] = useState<McpAuthMetadata | null>(null);
  const [runtimeServerUrl, setRuntimeServerUrl] = useState('');
  const [runtimeTokenStorageKey, setRuntimeTokenStorageKey] = useState('');

  // Grouped into one memo (§3.5 hook-deps budget: max 8 entries per
  // array) so downstream callbacks depend on ONE stable-per-`values`
  // reference instead of 6 individually-destructured primitives.
  const derived = useMemo(() => {
    const toolkitType = values?.type;
    return {
      toolkitId: values?.id,
      toolkitType,
      url: values?.settings?.url,
      clientId: values?.settings?.client_id,
      clientSecret: values?.settings?.client_secret,
      scopes: values?.settings?.scopes,
      isPrebuildMcp: isPrebuildMcpType(toolkitType),
    };
  }, [values]);

  const handleMcpAuthRequired = useCallback((message: AuthRequiredMessage | null | undefined) => {
    const metadata = extractMcpAuthMetadata(message);
    setMcpAuthMetadata(metadata);
    const serverUrl = message?.response_metadata?.server_url ?? '';
    setRuntimeServerUrl(serverUrl);

    const configUuid = metadata.configurationUuid;
    const oauthEndpoint = metadata.authServers?.[0];
    if (configUuid && oauthEndpoint) {
      setRuntimeTokenStorageKey(`${configUuid}:${oauthEndpoint}`);
    } else if (derived.toolkitType === 'mcp' || derived.isPrebuildMcp) {
      // MCP grants belong to the resource, not to every resource at an issuer.
      setRuntimeTokenStorageKey('');
    } else if (oauthEndpoint && oauthEndpoint !== serverUrl) {
      setRuntimeTokenStorageKey(oauthEndpoint);
    } else {
      setRuntimeTokenStorageKey('');
    }
    setShowModal(true);
  }, [derived]);

  const handleCloseModal = useCallback(
    (success?: boolean) => {
      setShowModal(false);
      setMcpAuthMetadata(null);
      setRuntimeServerUrl('');
      setRuntimeTokenStorageKey('');
      if (success) {
        onSuccess?.();
      }
    },
    [onSuccess],
  );

  const handleCancelModal = useCallback(() => {
    setShowModal(false);
    setMcpAuthMetadata(null);
    setRuntimeServerUrl('');
    setRuntimeTokenStorageKey('');
  }, []);

  const openModal = useCallback((metadata: McpAuthMetadata) => {
    setMcpAuthMetadata(metadata);
    setShowModal(true);
  }, []);

  const getModalProps = useCallback(
    (): McpAuthModalRenderProps => ({
      open: showModal && mcpAuthMetadata !== null,
      serverUrl: derived.url ?? runtimeServerUrl,
      tokenStorageKey: runtimeTokenStorageKey || undefined,
      mcpAuthMetadata,
      formClientId: derived.clientId,
      formClientSecret: derived.clientSecret,
      formScopes: derived.scopes,
      projectId,
      toolkitId: derived.toolkitId,
      toolkitType: derived.isPrebuildMcp || derived.toolkitType === 'mcp' ? derived.toolkitType : undefined,
      onClose: handleCloseModal,
      onCancel: handleCancelModal,
    }),
    [showModal, mcpAuthMetadata, derived, runtimeServerUrl, runtimeTokenStorageKey, projectId, handleCloseModal, handleCancelModal],
  );

  return { showModal, mcpAuthMetadata, runtimeServerUrl, handleMcpAuthRequired, handleCloseModal, handleCancelModal, openModal, getModalProps };
}

/** Config/credentials OAuth modal for `check_connection` 401 flows (baseline: `useConfigOAuthModal`). */
export interface UseConfigOAuthModalOptions {
  onSuccess?: (() => void) | undefined;
  credentials?: { client_id?: string | undefined; client_secret?: string | undefined; scopes?: string | readonly string[] | undefined } | undefined;
  toolkitId?: string | undefined;
  projectId?: string | number | undefined;
}

export interface UseConfigOAuthModalResult {
  showModal: boolean;
  handleConfigAuthRequired: (errorData: { auth_metadata?: ConfigAuthMetadataSource } | null | undefined, serverUrlOverride?: string, tokenStorageKeyOverride?: string) => void;
  getModalProps: () => McpAuthModalRenderProps & { title: string };
  handleCloseModal: (success?: boolean) => void;
  handleCancelModal: () => void;
}

export function useConfigOAuthModal(options: UseConfigOAuthModalOptions = {}): UseConfigOAuthModalResult {
  const { onSuccess, credentials, toolkitId, projectId } = options;
  // Grouped for the same hook-deps-budget reason as useMcpAuthModal's `derived`.
  const optionsSnapshot = useMemo(() => ({ credentials, toolkitId, projectId }), [credentials, toolkitId, projectId]);

  const [showModal, setShowModal] = useState(false);
  const [mcpAuthMetadata, setMcpAuthMetadata] = useState<McpAuthMetadata | null>(null);
  const [serverUrl, setServerUrl] = useState('');
  const [tokenStorageKey, setTokenStorageKey] = useState('');

  const handleConfigAuthRequired = useCallback(
    (errorData: { auth_metadata?: ConfigAuthMetadataSource } | null | undefined, serverUrlOverride?: string, tokenStorageKeyOverride?: string) => {
      const authMetadata = errorData?.auth_metadata;
      if (!authMetadata) return;
      const metadata = extractConfigAuthMetadata(authMetadata);
      if (!metadata) return;
      setMcpAuthMetadata(metadata);
      const resolvedServerUrl = serverUrlOverride ?? (authMetadata as { server_url?: string }).server_url ?? '';
      setServerUrl(resolvedServerUrl);
      setTokenStorageKey(tokenStorageKeyOverride ?? serverUrlOverride ?? resolvedServerUrl);
      setShowModal(true);
    },
    [],
  );

  const handleCloseModal = useCallback(
    (success?: boolean) => {
      setShowModal(false);
      setMcpAuthMetadata(null);
      setServerUrl('');
      setTokenStorageKey('');
      if (success) onSuccess?.();
    },
    [onSuccess],
  );

  const handleCancelModal = useCallback(() => {
    setShowModal(false);
    setMcpAuthMetadata(null);
    setServerUrl('');
    setTokenStorageKey('');
  }, []);

  const getModalProps = useCallback(
    () => ({
      open: showModal && mcpAuthMetadata !== null,
      serverUrl,
      tokenStorageKey,
      mcpAuthMetadata,
      formClientId: optionsSnapshot.credentials?.client_id,
      formClientSecret: optionsSnapshot.credentials?.client_secret,
      formScopes: optionsSnapshot.credentials?.scopes,
      projectId: optionsSnapshot.projectId,
      toolkitId: optionsSnapshot.toolkitId,
      toolkitType: undefined,
      title: 'Configuration OAuth',
      onClose: handleCloseModal,
      onCancel: handleCancelModal,
    }),
    [showModal, mcpAuthMetadata, serverUrl, tokenStorageKey, optionsSnapshot, handleCloseModal, handleCancelModal],
  );

  return { showModal, handleConfigAuthRequired, getModalProps, handleCloseModal, handleCancelModal };
}
