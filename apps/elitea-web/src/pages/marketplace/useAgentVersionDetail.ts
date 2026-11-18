/**
 * useAgentVersionDetail — fetches an agent's full version detail (welcome
 * message, conversation starters, and everything else `ApplicationVersionDetail`
 * carries) for `AgentModal`.
 *
 * Adversarial-review fix, cluster A13-agents-hub, finding 2: `AgentModal`
 * previously never fetched this at all, so its Welcome Message and
 * Conversation Starters sections always rendered their empty-state
 * fallback regardless of what was actually configured for the agent.
 *
 * Uses the REAL generated `useGetPublicApplication(applicationId,
 * versionName)` hook — `GET /elitea_core/public_application/prompt_lib/
 * {applicationId}/{versionName}`, `internal/api/v2/eliteacore/handler.go`'s
 * `publicApplicationDetail` (:1319-1483). `versionName` is read straight off
 * the `ApplicationData` the hub already fetched (`version_name`, always
 * populated on every row of the bulk-list response) — no extra lookup is
 * needed to know which version to ask for.
 *
 * (There IS also a no-`versionName` variant of this route registered on the
 * real router — `router.go:613`, `/public_application/prompt_lib/
 * {applicationID}` — which resolves to whichever version is currently
 * published. It is NOT in the OpenAPI spec / generated client, only the
 * versioned route is. Since every `ApplicationData` this cluster ever
 * constructs already carries a real `version_name`, the documented,
 * generated endpoint is sufficient here and no raw-fetch backend-gap
 * workaround is needed — unlike `useAgentHubData.ts`'s bulk-list case.)
 */
import { useMemo } from 'react';

import { useGetPublicApplication } from '@/shared/api/generated/applications/applications';
import type { ApplicationVersionDetail } from '@/shared/api/generated/model';

export interface UseAgentVersionDetailResult {
  readonly versionDetails: ApplicationVersionDetail | undefined;
  readonly isLoading: boolean;
}

export function useAgentVersionDetail(applicationId: string, versionName: string): UseAgentVersionDetailResult {
  const numericId = Number(applicationId);
  const query = useGetPublicApplication(numericId, versionName, {
    query: { enabled: Number.isFinite(numericId) && versionName !== '' },
  });

  const versionDetails = useMemo(() => {
    const envelope = query.data;
    if (!envelope || envelope.status !== 200) return undefined;
    return envelope.data.version_details;
  }, [query.data]);

  return { versionDetails, isLoading: query.isFetching };
}
