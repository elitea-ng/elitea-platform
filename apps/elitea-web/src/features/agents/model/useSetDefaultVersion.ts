import { useCallback, useState } from 'react';

import { useQueryClient } from '@tanstack/react-query';

import {
  getGetApplicationQueryKey,
  getSetApplicationDefaultVersionQueryOptions,
} from '@/shared/api/generated/applications/applications';
import type { OkResponse } from '@/shared/api/generated/model';

import { applicationErrorMessage } from '../lib/errorMessage';

/**
 * Port of `apps/elitea-ui/src/[fsd]/entities/version/lib/hooks/
 * useSetDefaultVersion.hooks.jsx` — the data half of #147 ("the agent version
 * menu has no set-default item"). The route, the handler and the generated
 * client all existed; nothing called them.
 *
 * `PATCH /elitea_core/default_version/prompt_lib/{projectId}/{applicationId}/
 * {versionId}` → `SetDefaultVersion`
 * (`services/elitea-main/internal/api/v2/applications/handler.go:1202-1225`)
 * → `ApplicationsRepo.SetDefaultVersion`
 * (`internal/infra/db/repos/applications.go:650-682`), read in full:
 *
 *  - The version id travels in the PATH. The optional body `{"version_id":
 *    "..."}` OVERRIDES it and exists for old-SPA compatibility only
 *    (:1206-1213); this hook sends no body, so the path segment decides. The
 *    baseline sends the 3-segment form with the id in the body, which the Go
 *    router answers with a 405 — that shape is deliberately not ported.
 *  - The repo refuses a version that does not belong to this application
 *    (404 "version not found") and an application that does not exist (404),
 *    so a wrong id fails loudly rather than writing a dangling reference.
 *  - Success writes `applications.meta.default_version_id` and answers
 *    `{"ok": true}`.
 *
 * **`staleTime: 0` is load-bearing, not decoration, and it has nothing to do
 * with reading the default back.** The generated client models this PATCH as
 * a QUERY, so it goes through `queryClient.fetchQuery`, and the app's own
 * client sets `staleTime: 30_000` (`app/providers/queryClient.ts:101`).
 * Without the override, setting the default to version A, then to B, then
 * back to A inside 30 seconds would replay A's cache entry and send NO second
 * request, while still reporting success. That is a property of how this
 * mutation is modelled; do not remove it.
 *
 * **No toast, no cache surgery, no navigation** — same caller-owns-
 * orchestration posture as `useDeleteVersion`/`useSaveNewVersion`. The
 * baseline patches its RTK-Query detail cache to record the new default; here
 * the caller keeps the id it just set as a short-lived override until the
 * detail is re-fetched. That override is now only an override: `GET
 * /application/...` does report the default back — `meta.default_version_id`
 * on the application and `is_default` on each `versions[]` entry
 * (`applications/handler.go`'s `Get`/`getVersions`) — so a reload shows the
 * right version without this hook having to invalidate anything. See
 * `AgentVersionControls`.
 */
export interface UseSetDefaultVersionInput {
  readonly projectId: string;
  readonly applicationId: number;
}

export interface UseSetDefaultVersionResult {
  readonly doSetDefaultVersion: (versionId: number) => Promise<boolean>;
  readonly isSettingDefaultVersion: boolean;
  readonly error: unknown;
  readonly errorMessage: string | undefined;
  /**
   * Drops a refusal the caller has finished showing — the baseline's own
   * `reset()` (`useSetDefaultVersion.hooks.jsx:79,87`). Without it a caller
   * that keeps this hook mounted across dialog openings (the version bar
   * does) re-shows the previous attempt's error the moment the dialog opens
   * again, before anything has been sent.
   */
  readonly resetError: () => void;
}

export function useSetDefaultVersion(input: UseSetDefaultVersionInput): UseSetDefaultVersionResult {
  const queryClient = useQueryClient();
  const [isSettingDefaultVersion, setIsSettingDefaultVersion] = useState(false);
  const [error, setError] = useState<unknown>(undefined);

  const doSetDefaultVersion = useCallback(
    async (versionId: number): Promise<boolean> => {
      setIsSettingDefaultVersion(true);
      setError(undefined);
      try {
        const options = getSetApplicationDefaultVersionQueryOptions(input.projectId, input.applicationId, versionId);
        const response = await queryClient.query({ ...options, staleTime: 0 });
        const ok = (response as { data: OkResponse }).data.ok;
        // #1991: the application detail is cached (30 s) and embeds the
        // DEFAULT version's `version_details` and each row's `is_default`.
        // Leaving it alone meant leaving the agent and opening it again
        // inside the window replayed the old default (usually `base`) and the
        // pinned version was neither selected nor listed first.
        if (ok) await queryClient.invalidateQueries({ queryKey: getGetApplicationQueryKey(input.projectId, input.applicationId) });
        return ok;
      } catch (caught) {
        setError(caught);
        return false;
      } finally {
        setIsSettingDefaultVersion(false);
      }
    },
    [input.applicationId, input.projectId, queryClient],
  );

  const resetError = useCallback(() => setError(undefined), []);

  return {
    doSetDefaultVersion,
    isSettingDefaultVersion,
    error,
    errorMessage: error === undefined ? undefined : applicationErrorMessage(error),
    resetError,
  };
}
