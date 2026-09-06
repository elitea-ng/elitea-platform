import { useCallback, useState } from 'react';

import { useQueryClient } from '@tanstack/react-query';

import { selectDefaultVersion } from '@/entities/version';
import type { VersionSummary } from '@/entities/version';
import {
  getBatchReplaceVersionReferencesQueryOptions,
  getCheckVersionInUseQueryOptions,
  getDeleteApplicationVersionQueryOptions,
} from '@/shared/api/generated/applications/applications';
import type { ApplicationRelationList, OkResponse } from '@/shared/api/generated/model';

import { t } from '@/shared/i18n';

import { applicationServerErrorMessage } from '../lib/errorMessage';

/**
 * The message shown when the server refused the delete and explained nothing.
 * A function, not a module constant: `t` reads the loaded bundle, and a
 * module-level call would freeze the string at import time.
 */
function deleteFailedFallback(): string {
  return t('features.agents.deleteVersion.error', 'Failed to delete this version.');
}

/**
 * Port of `apps/elitea-ui/src/hooks/application/useDeleteVersion.js`.
 *
 * **`doCheckVersionInUse` — real response shape, not the baseline's.** The
 * baseline's `useLazyCheckVersionInUseQuery` implies a boolean/count
 * response; the REAL endpoint (`GET /check_version_in_use/prompt_lib/...`)
 * is routed at `ApplicationRelation`
 * (`services/elitea-main/internal/api/router.go:509` →
 * `internal/api/v2/eliteacore/handler.go:1590-1626`, confirmed by reading
 * both), which returns the list of `skill`/`tool` rows in
 * `entity_skill_mapping`/`entity_tool_mapping` that reference this version —
 * `{items: [{type, id}]}` (`ApplicationRelationList`), not a boolean. This
 * is actually MORE useful than the baseline's opaque in-use flag: `items`
 * tells a caller WHAT references the version, not just that something does.
 * `isInUse` (`items.length > 0`) is provided for a caller that only wants
 * the baseline's boolean.
 *
 * **`doDeleteVersion` — the baseline's single combined "delete with optional
 * replacement" call does not work the way it assumes; traced to real Go
 * source, not guessed:**
 *
 * `DeleteApplicationVersionParams.replacement_version_id` IS a real,
 * documented query param on `DELETE /version/prompt_lib/.../{versionId}` —
 * but reading `DeleteVersion`
 * (`services/elitea-main/internal/api/v2/applications/handler.go:932-957`)
 * in full shows it is NEVER READ: the handler only checks the
 * published/embedded guard and calls `h.repo.DeleteVersion(...)`, nothing
 * else. Passing `replacement_version_id` to this endpoint is a silent
 * no-op — the "atomically replace references then delete" behaviour the
 * baseline's single-dialog flow relies on simply does not happen through
 * this call.
 *
 * The REAL endpoint for that combined operation is a DIFFERENT one:
 * `POST /batch_replace_version/prompt_lib/{projectId}/{oldVersionId}/
 * {newVersionId}?delete_old=true|false` → `BatchReplaceVersion`
 * (`applications/handler.go:984-995` → repo `BatchReplaceVersion`,
 * `internal/infra/db/repos/applications.go:410-425`), which repoints ONLY
 * `entity_tool_mapping` rows from `oldVersionId` to `newVersionId` and, when
 * `delete_old=true`, ALSO deletes the old version — confirmed by reading the
 * repo method directly: its single `UPDATE` statement targets
 * `entity_tool_mapping` and nothing else, there is no matching
 * `entity_skill_mapping` statement anywhere in that function. So a version
 * referenced by a skill (an `entity_skill_mapping` row) is NOT repointed by
 * this call: deleting/replacing such a version via
 * `batchReplaceVersionReferences` leaves that skill's mapping pointing at a
 * row that (when `delete_old=true`) no longer exists — an orphaned
 * reference this hook cannot detect or repair, because
 * `doCheckVersionInUse` (below) reports the `skill`/`tool` rows correctly
 * but nothing here acts differently for a `skill`-typed reference before
 * calling `doDeleteVersion`. Responds `{"ok": true}`
 * (`shared/api/generated/model/okResponse.zod.ts` — this is the operation
 * that schema's own doc comment cites `BatchReplaceVersion (:994)` for).
 * `doDeleteVersion(replacementVersionId)` therefore branches on whether a
 * replacement was supplied:
 * - with one: `batchReplaceVersionReferences(..., {delete_old: true})` —
 *   replaces AND deletes in the one real atomic call.
 * - without one: plain `deleteApplicationVersion` (204, no body — NOT the
 *   baseline's `{ok, updated_references}`; there is no reference count on
 *   the real response, so this hook cannot report one).
 *
 * **Fallback-version selection is NOT re-derived here.** The baseline's
 * `useReplaceVersionInPath`'s "which version to fall back to" `useMemo`
 * (`useDeleteVersion.js:10-34`) was already promoted into
 * `entities/version`'s `selectDefaultVersion` — that function's OWN doc
 * comment names this exact file (`hooks/application/useDeleteVersion.js:
 * 21-23`) as one of its three baseline call sites. `resolveFallbackVersionId`
 * below is a two-line wrapper (exclude the version being deleted, then
 * delegate) rather than a duplicate implementation.
 *
 * **No navigation, no `useLocation`/`useParams`/toast** — same
 * caller-owns-orchestration redesign as `useCreateApplication.ts` (point 4);
 * this hook returns success/failure and the resolved fallback id, the
 * caller (which owns the router) does the actual navigate.
 */

/** Not exported — no consumer outside this file needs it yet (knip: unused-export discipline); the `doCheckVersionInUse` field it types IS exported via `UseDeleteVersionResult`, so the shape is still reachable through that. */
interface CheckVersionInUseResult {
  readonly items: ApplicationRelationList['items'];
  readonly isInUse: boolean;
}

export interface UseDeleteVersionInput {
  readonly projectId: string;
  readonly applicationId: number;
  readonly versionId: number;
}

/**
 * What one delete attempt answered.
 *
 * `errorMessage` IS ON THE OUTCOME, not only in the hook's state, and that is
 * the point (#147). `error` below is React state: a caller that reads
 * `errorMessage` from its own closure straight after `await doDeleteVersion()`
 * reads the value from the render it was created in, which is the value from
 * BEFORE the failure. `DeleteVersionButton` did exactly that, so the server's
 * own refusal ("Unpublish first. Cannot delete a published version.") was
 * replaced by the generic fallback on every first failure. The state fields
 * stay, for a caller that renders the message reactively.
 *
 * Not exported: the shape is reachable through `UseDeleteVersionResult`, and
 * knip flags an unused named export — the same discipline
 * `CheckVersionInUseResult` above records.
 */
interface DeleteVersionOutcome {
  readonly ok: boolean;
  readonly errorMessage: string | undefined;
}

export interface UseDeleteVersionResult {
  readonly doCheckVersionInUse: () => Promise<CheckVersionInUseResult | undefined>;
  readonly isCheckingInUse: boolean;
  /** `undefined` when a replacement version id is supplied — deletion happens via the batch-replace endpoint in that case, not this one; see the module doc comment. */
  readonly doDeleteVersion: (replacementVersionId?: number) => Promise<DeleteVersionOutcome>;
  readonly isDeletingVersion: boolean;
  readonly error: unknown;
  readonly errorMessage: string | undefined;
}

/** `useDeleteVersion.js:10-34`'s fallback-selection, now a thin wrapper over the promoted `entities/version` selector — see the module doc comment. */
export function resolveFallbackVersionId(
  versions: readonly VersionSummary[],
  currentVersionId: string,
  defaultVersionId: string | undefined,
): string | undefined {
  const candidates = versions.filter((version) => version.id !== currentVersionId);
  return selectDefaultVersion(candidates, defaultVersionId)?.id ?? candidates[0]?.id;
}

export function useDeleteVersion(input: UseDeleteVersionInput): UseDeleteVersionResult {
  const queryClient = useQueryClient();
  const [isCheckingInUse, setIsCheckingInUse] = useState(false);
  const [isDeletingVersion, setIsDeletingVersion] = useState(false);
  const [error, setError] = useState<unknown>(undefined);

  const doCheckVersionInUse = useCallback(async (): Promise<CheckVersionInUseResult | undefined> => {
    setIsCheckingInUse(true);
    setError(undefined);
    try {
      const options = getCheckVersionInUseQueryOptions(input.projectId, input.applicationId, input.versionId);
      const response = await queryClient.query(options);
      const { items } = (response as { data: ApplicationRelationList }).data;
      return { items, isInUse: items.length > 0 };
    } catch (caught) {
      setError(caught);
      return undefined;
    } finally {
      setIsCheckingInUse(false);
    }
  }, [input.applicationId, input.projectId, input.versionId, queryClient]);

  const doDeleteVersion = useCallback(
    async (replacementVersionId?: number): Promise<DeleteVersionOutcome> => {
      setIsDeletingVersion(true);
      setError(undefined);
      try {
        if (replacementVersionId !== undefined) {
          const options = getBatchReplaceVersionReferencesQueryOptions(
            input.projectId,
            input.versionId,
            replacementVersionId,
            { delete_old: true },
          );
          const response = await queryClient.query(options);
          return { ok: (response as { data: OkResponse }).data.ok, errorMessage: undefined };
        }
        await queryClient.query(
          getDeleteApplicationVersionQueryOptions(input.projectId, input.applicationId, input.versionId),
        );
        return { ok: true, errorMessage: undefined };
      } catch (caught) {
        setError(caught);
        return { ok: false, errorMessage: applicationServerErrorMessage(caught, deleteFailedFallback()) };
      } finally {
        setIsDeletingVersion(false);
      }
    },
    [input.applicationId, input.projectId, input.versionId, queryClient],
  );

  return {
    doCheckVersionInUse,
    isCheckingInUse,
    doDeleteVersion,
    isDeletingVersion,
    error,
    errorMessage: error === undefined ? undefined : applicationServerErrorMessage(error, deleteFailedFallback()),
  };
}
