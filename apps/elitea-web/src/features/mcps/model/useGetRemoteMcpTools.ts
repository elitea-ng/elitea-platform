/**
 * Port of
 * apps/elitea-ui/src/[fsd]/features/mcp/lib/hooks/useGetRemoteMcpTools.hooks.js
 * (unit A5) — fetches MCP tools via `mcp_sync_tools` (works for both remote
 * MCPs, keyed by `serverUrl`, and pre-built MCPs, keyed by `toolkitType`),
 * retrying automatically once OAuth completes.
 *
 * DEVIATIONS FROM BASELINE:
 *  - `useMcpSyncToolsMutation` (RTK Query) -> `api/mcpSyncTools.ts`'s plain
 *    async function (this app has no RTK Query; react-query is the data
 *    layer, but this call is imperative/one-shot, not a component-level
 *    cached query, so a bare async call is the right shape here — same
 *    posture `oauthFlow.ts`/`tokenLifecycle.ts` already take).
 *  - `SocketContext` -> `useSocketClient().socket.id` (S5's typed client
 *    exposes the raw socket as an escape hatch for exactly this kind of
 *    read).
 *  - `useToast` -> `onToolsFetched`/`onError` callbacks (see
 *    `useMcpAuthModal.ts`'s header for the toast-removal rationale).
 *  - `projectId` is an explicit option (same rationale as the sibling hooks).
 */
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';

import { useSocketClient } from '@/shared/api/socket/client';

import type { McpSyncToolsParams, McpSyncToolsResponse } from '../api/mcpSyncTools';
import { mcpSyncTools } from '../api/mcpSyncTools';
import { isPrebuildMcpType, setConnectionVerified } from '../lib/storage';
import { getExecutionTokens } from '../lib/executionTokens';

import type { UseMcpAuthCheckValues } from './useMcpAuthCheck';
import { useMcpAuthModal } from './useMcpAuthModal';
import type { McpAuthModalRenderProps, McpAuthModalValues } from './useMcpAuthModal';

export interface UseGetRemoteMcpToolsOptions {
  values?: (McpAuthModalValues & UseMcpAuthCheckValues & { settings?: { timeout?: number | undefined; ssl_verify?: boolean | undefined } }) | undefined;
  toolkitType?: string | undefined;
  projectId?: string | number | undefined;
  onToolsFetched?: ((tools: readonly unknown[], argsSchemas: Record<string, unknown> | undefined) => void) | undefined;
  onError?: ((message: string) => void) | undefined;
  onSuccess?: ((toolCount: number) => void) | undefined;
}

export interface UseGetRemoteMcpToolsResult {
  fetchTools: () => void;
  isLoading: boolean;
  getModalProps: () => McpAuthModalRenderProps;
}

export function useGetRemoteMcpTools(options: UseGetRemoteMcpToolsOptions): UseGetRemoteMcpToolsResult {
  const { values, toolkitType, projectId, onToolsFetched, onError, onSuccess } = options;
  const [isLoading, setIsLoading] = useState(false);
  const socket = useSocketClient();

  const onToolsFetchedRef = useRef(onToolsFetched);
  const pendingRetryRef = useRef(false);
  useEffect(() => {
    onToolsFetchedRef.current = onToolsFetched;
  }, [onToolsFetched]);

  // Grouped into one memo (§3.5 hook-deps budget) — both fields derive
  // from the same two inputs, so they belong in one dependency slot.
  const mcpMeta = useMemo(() => {
    const effectiveToolkitType = toolkitType ?? values?.type;
    return { isPrebuildMcp: isPrebuildMcpType(effectiveToolkitType), effectiveToolkitType, serverUrl: values?.settings?.url };
  }, [toolkitType, values?.type, values?.settings?.url]);
  const currentRequest = useRef('');
  currentRequest.current = JSON.stringify({ values, mcpMeta, projectId });

  const onAuthSuccess = useCallback(() => {
    pendingRetryRef.current = true;
  }, []);

  const valuesWithType = useMemo(() => ({ ...values, type: mcpMeta.effectiveToolkitType }), [values, mcpMeta.effectiveToolkitType]);

  const { handleMcpAuthRequired, getModalProps, showModal } = useMcpAuthModal({ onSuccess: onAuthSuccess, values: valuesWithType, projectId });

  const executeFetch = useCallback(async () => {
    if (isLoading) return;
    const { isPrebuildMcp, effectiveToolkitType, serverUrl } = mcpMeta;

    const validationError = validateSyncToolsInputs({ serverUrl, isPrebuildMcp, effectiveToolkitType });
    if (validationError) {
      onError?.(validationError);
      return;
    }
    if (projectId === undefined) {
      onError?.('A project is required to load MCP tools');
      return;
    }

    setIsLoading(true);
    const requestIdentity = JSON.stringify({ values, mcpMeta, projectId });
    try {
      const mcpTokens = await discoveryTokens(String(projectId), mcpMeta);
      if (currentRequest.current !== requestIdentity) return;
      const requestParams = buildSyncToolsRequestParams(values, mcpMeta, projectId, socket.socket.id, mcpTokens);
      const response = await mcpSyncTools(requestParams);
      if (currentRequest.current !== requestIdentity) return;

      applySyncToolsOutcome(classifySyncToolsResult(response), {
        handleMcpAuthRequired,
        onToolsFetchedRef,
        onSuccess,
        onError,
        markConnectionVerified: () => markToolsFetchConnectionVerified(isPrebuildMcp, effectiveToolkitType, serverUrl),
      });
    } catch (error) {
      if (currentRequest.current !== requestIdentity) return;
      onError?.(error instanceof Error ? error.message : 'Failed to fetch tools');
    } finally {
      setIsLoading(false);
    }
  }, [isLoading, values, projectId, handleMcpAuthRequired, onError, onSuccess, socket, mcpMeta]);

  useEffect(() => {
    if (!showModal && pendingRetryRef.current) {
      pendingRetryRef.current = false;
      void executeFetch();
    }
    return undefined;
  }, [showModal, executeFetch]);

  const fetchTools = useCallback(() => {
    const serverUrl = values?.settings?.url;
    if (!serverUrl && !mcpMeta.isPrebuildMcp) {
      onError?.('MCP server URL is required');
      return;
    }
    void executeFetch();
  }, [values?.settings?.url, mcpMeta, executeFetch, onError]);

  return { fetchTools, isLoading, getModalProps };
}

/** Resource-scoped preview uses the same refresh contract as execution. */
async function discoveryTokens(projectId: string, { serverUrl, isPrebuildMcp, effectiveToolkitType }: { serverUrl: string | undefined; isPrebuildMcp: boolean; effectiveToolkitType: string | undefined }): Promise<Record<string, unknown>> {
  const tokens = await getExecutionTokens(projectId);
  const prebuiltAlias = isPrebuildMcp ? effectiveToolkitType : undefined;
  const key = serverUrl && tokens[serverUrl] ? serverUrl : prebuiltAlias;
  return key && tokens[key] ? { [key]: tokens[key] } : {};
}

/** Maps a couple of common raw error substrings to friendlier copy (baseline: `useGetRemoteMcpTools.hooks.js:122-129`). */
function describeSyncError(rawError: string): string {
  if (rawError.includes('403') || rawError.includes('Forbidden')) {
    return 'Access denied. The OAuth token may have insufficient permissions or the server rejected the request. Please try re-authorizing with the correct scopes.';
  }
  if (rawError.includes('401') || rawError.includes('Unauthorized')) {
    return 'Authorization failed. Please try logging in again.';
  }
  return rawError;
}

/**
 * Pre-flight validation split out of `executeFetch` (§3.5 complexity budget:
 * the inlined form measured 36). Pure — returns the user-facing message, or
 * `null` when the request may proceed.
 */
function validateSyncToolsInputs(input: { serverUrl: string | undefined; isPrebuildMcp: boolean; effectiveToolkitType: string | undefined }): string | null {
  if (!input.serverUrl && !input.isPrebuildMcp) {
    return 'MCP server URL is required';
  }
  if (input.isPrebuildMcp && !input.effectiveToolkitType) {
    return 'Toolkit type is required for pre-built MCP';
  }
  return null;
}

/**
 * Builds the `mcp_sync_tools` request body from the hook's `values`/`mcpMeta`
 * — split out of `executeFetch` (§3.5 complexity budget: even after the
 * validation/outcome-classification extractions above, the inlined request
 * object still measured 19, mostly from four repeated `values?.settings?.x`
 * chains).
 */
function buildSyncToolsRequestParams(
  values: UseGetRemoteMcpToolsOptions['values'],
  mcpMeta: { isPrebuildMcp: boolean; effectiveToolkitType: string | undefined },
  projectId: string | number,
  sid: string | undefined,
  mcpTokens: Record<string, unknown>,
): McpSyncToolsParams {
  const settings = values?.settings;
  return {
    projectId,
    url: settings?.url,
    headers: settings?.headers,
    timeout: settings?.timeout ?? 60,
    mcp_tokens: Object.keys(mcpTokens).length > 0 ? mcpTokens : undefined,
    sid,
    ssl_verify: settings?.ssl_verify,
    toolkit_type: mcpMeta.isPrebuildMcp ? mcpMeta.effectiveToolkitType : undefined,
  };
}

type SyncToolsOutcome =
  | { kind: 'auth_required'; responseMetadata: Record<string, unknown> }
  | { kind: 'tools'; tools: readonly unknown[]; argsSchemas: Record<string, unknown> | undefined }
  | { kind: 'error'; message: string };

/**
 * Pure classification of a `mcp_sync_tools` response into the three outcomes
 * `executeFetch` used to branch on inline (baseline: `response.result ??
 * response`, `useGetRemoteMcpTools.hooks.js:94-118`). Split out for the same
 * complexity-budget reason as `validateSyncToolsInputs`.
 */
function classifySyncToolsResult(response: McpSyncToolsResponse): SyncToolsOutcome {
  const result = response.result ?? response;

  if (result.requires_authorization) {
    return { kind: 'auth_required', responseMetadata: result.response_metadata ?? {} };
  }
  if (result.success && result.tools) {
    return { kind: 'tools', tools: result.tools, argsSchemas: result.args_schemas };
  }
  if (result.success === false && result.error) {
    return { kind: 'error', message: describeSyncError(result.error) };
  }
  if (result.error) {
    return { kind: 'error', message: result.error };
  }
  return { kind: 'error', message: 'Failed to fetch tools: Unknown response format' };
}

interface SyncToolsOutcomeHandlers {
  handleMcpAuthRequired: (source: { response_metadata: Record<string, unknown> }) => void;
  onToolsFetchedRef: { current: ((tools: readonly unknown[], argsSchemas: Record<string, unknown> | undefined) => void) | undefined };
  /** Marks the server "connection verified" in storage — see `markToolsFetchConnectionVerified`'s doc comment. */
  markConnectionVerified: () => void;
  onSuccess: ((toolCount: number) => void) | undefined;
  onError: ((message: string) => void) | undefined;
}

/** Applies a classified outcome to the hook's side-effecting callbacks. */
function applySyncToolsOutcome(outcome: SyncToolsOutcome, handlers: SyncToolsOutcomeHandlers): void {
  switch (outcome.kind) {
    case 'auth_required':
      handlers.handleMcpAuthRequired({ response_metadata: outcome.responseMetadata });
      return;
    case 'tools':
      handlers.onToolsFetchedRef.current?.(outcome.tools, outcome.argsSchemas);
      handlers.markConnectionVerified();
      handlers.onSuccess?.(outcome.tools.length);
      return;
    case 'error':
      handlers.onError?.(outcome.message);
      return;
  }
}

/**
 * Marks a header-based-auth (non-OAuth) MCP server "connection verified"
 * after `mcp_sync_tools` succeeds — mirrors `useMcpLogin.ts`'s
 * `handleConnectionSuccess` and the baseline
 * (`useGetRemoteMcpTools.hooks.js:112-117`,
 * `McpAuthHelpers.setConnectionVerified`). A successful tools fetch for a
 * server that needs no client-side OAuth token never goes through
 * `startMcpAuthFlow` (the only other call site that stores a token), so
 * without this, `useMcpTokenChange`'s `isLoggedIn` — and the
 * login/connected UI it drives for THIS hook's own caller — would never
 * flip for that server, even though tools were just fetched successfully.
 * A no-op for an OAuth-backed remote MCP that already has a real token:
 * `setConnectionVerified` itself refuses to overwrite one (`storage.ts`).
 */
function markToolsFetchConnectionVerified(isPrebuildMcp: boolean, effectiveToolkitType: string | undefined, serverUrl: string | undefined): void {
  if (isPrebuildMcp) {
    setConnectionVerified(undefined, effectiveToolkitType);
  } else if (serverUrl) {
    setConnectionVerified(serverUrl);
  }
}
