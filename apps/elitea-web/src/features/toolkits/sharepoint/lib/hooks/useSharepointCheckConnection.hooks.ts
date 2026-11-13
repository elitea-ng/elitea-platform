import { useCallback, useState } from 'react';

import { EliteaApiError } from '@/shared/api/generated/mutator';
import { serverErrorSentence } from '@/shared/api/serverErrorMessage';
import { t } from '@/shared/i18n';

import { testConfigurationConnection } from '../api/configurations';
import type { SharepointResolvedConfig } from './useResolvedSharepointConfig.hooks';

/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/sharepoint/lib/hooks/
 * useSharepointCheckConnection.hooks.js` (Wave-2 unit A4e). `spConfig`/
 * `projectId`/`onSuccess`/`runCheck(handleConfigAuthRequired, tokenStorageKey)`
 * signatures kept identical to the baseline — this hook was ALREADY
 * decoupled from any concrete auth-modal implementation in the baseline
 * (it takes `handleConfigAuthRequired` as a plain callback parameter, not an
 * import), so no redesign is needed here for the `no-sideways-features`
 * constraint; `../ui/SharepointDelegatedLoginButton.tsx`/
 * `../ui/SharepointOAuthStatus.tsx` are what supply that callback from
 * their own local `useSharepointAuthModal` hook.
 *
 * ONE REAL, DISCLOSED GAP carried over unfixed from the baseline (not
 * introduced by this port): `error?.status === 401 && error?.data?.
 * requires_authorization` — this app's `eliteaFetch`/`http.ts` (§3.6)
 * treats EVERY 401 as a re-auth signal (`needsReauth`'s unconditional
 * `status === 401` check) and, on failed/absent re-auth, resolves to
 * `EliteaApiError` with `failure.kind === 'auth'` and NO response body —
 * the exact same shape gap `features/agents/model/useCreateConfiguration.ts`'s
 * own `isAuthRequiredError` doc comment already discloses for the identical
 * `/configurations/check_connection` endpoint. `isAuthRequiredError` below
 * checks both the (currently unreachable in practice) `kind: 'http'` body
 * shape AND the plain-object shape that file's precedent checks, so this
 * stays forward-compatible with a future `http.ts` fix without silently
 * pretending the gap does not exist today.
 */
export interface UseSharepointCheckConnectionInput {
  readonly projectId: string | undefined;
  readonly spConfig: SharepointResolvedConfig | null;
  /** Invoked on a successful connection test — e.g. `McpAuthHelpers.setConnectionVerified`'s local equivalent for the non-delegated (header-auth) case. */
  readonly onSuccess?: () => void;
}

export interface UseSharepointCheckConnectionResult {
  readonly runCheck: (
    handleConfigAuthRequired?: (errorData: unknown, serverUrlOverride?: string, tokenStorageKeyOverride?: string) => void,
    tokenStorageKey?: string,
  ) => Promise<void>;
  readonly isRunning: boolean;
  /** Why the last check did not connect, when it was neither a success nor a request to authorise (#1998); cleared by the next run. */
  readonly error: string | undefined;
}

interface EliteaApiErrorLike {
  readonly failure?: { readonly kind?: string; readonly status?: number; readonly body?: unknown };
}

function isEliteaApiErrorLike(value: unknown): value is EliteaApiErrorLike {
  return typeof value === 'object' && value !== null && 'failure' in value;
}

/**
 * Baseline: `error?.status === 401 && error?.data?.requires_authorization`.
 * See module doc comment for the real gap this checks around. Exported (not
 * just used internally) so the pure detection logic can be unit-tested
 * directly against a hand-built `EliteaApiError`-shaped input — the REAL
 * `eliteaFetch`/`http.ts` path cannot exercise the `kind: 'http'` branch
 * today (see `useSharepointCheckConnection.hooks.test.ts`'s own "REAL,
 * CURRENT GAP" describe block for the live-network proof of that).
 */
export function authRequiredErrorData(caught: unknown): unknown {
  if (isEliteaApiErrorLike(caught)) {
    const body = caught.failure?.body;
    if (
      caught.failure?.status === 401 &&
      typeof body === 'object' &&
      body !== null &&
      (body as { readonly requires_authorization?: unknown }).requires_authorization === true
    ) {
      return body;
    }
    return undefined;
  }
  if (
    typeof caught === 'object' &&
    caught !== null &&
    (caught as { readonly requires_authorization?: unknown }).requires_authorization === true
  ) {
    return caught;
  }
  return undefined;
}

export function useSharepointCheckConnection(input: UseSharepointCheckConnectionInput): UseSharepointCheckConnectionResult {
  const { projectId, spConfig, onSuccess } = input;
  const [isRunning, setIsRunning] = useState(false);
  const [error, setError] = useState<string | undefined>(undefined);

  const runCheck = useCallback(
    async (
      handleConfigAuthRequired?: (errorData: unknown, serverUrlOverride?: string, tokenStorageKeyOverride?: string) => void,
      tokenStorageKey?: string,
    ) => {
      if (isRunning || !spConfig || !projectId) return;
      setIsRunning(true);
      setError(undefined);
      try {
        await testConfigurationConnection(projectId, 'sharepoint', spConfig);
        onSuccess?.();
      } catch (caught) {
        const errorData = authRequiredErrorData(caught);
        if (errorData !== undefined) {
          const discoveryEndpoint = spConfig.oauth_discovery_endpoint;
          handleConfigAuthRequired?.(errorData, discoveryEndpoint, tokenStorageKey ?? discoveryEndpoint);
        } else {
          // The baseline ignored every other failure, so "Login" answered with
          // nothing at all when the server could not start the sign-in (#1998).
          const explained = caught instanceof EliteaApiError && caught.failure.kind === 'http' ? serverErrorSentence(caught.failure.body) : undefined;
          setError(explained ?? t('features.toolkits.sharepointOAuthStatus.checkFailed', 'Could not start the sign-in for this credential. Check its settings and try again.'));
        }
      } finally {
        setIsRunning(false);
      }
    },
    [isRunning, spConfig, projectId, onSuccess],
  );

  return { runCheck, isRunning, error };
}
