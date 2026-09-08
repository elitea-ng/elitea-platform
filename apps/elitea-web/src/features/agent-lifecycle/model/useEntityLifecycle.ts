import { useCallback, useMemo } from 'react';

import { useMutation, useQueryClient, type UseMutationResult } from '@tanstack/react-query';

import { getGetApplicationQueryKey } from '@/shared/api/generated/applications/applications';
import { EliteaApiError } from '@/shared/api/generated/mutator';
import { t } from '@/shared/i18n';

import {
  fetchExportDocument,
  fetchForkDocument,
  forkEntity,
  importEntities,
  publishVersion,
  runPublishValidation,
  unpublishVersion,
  type EntityExportDocument,
  type ForkOutcome,
  type ImportOutcome,
  type PublishOutcome,
} from '../api/lifecycleApi';
import {
  normalizePublishValidation,
  type NormalizedPublishValidation,
  type PublishCategory,
} from '../lib/publishValidation';

/**
 * The write half of the lifecycle plane.
 *
 * Each mutation invalidates the SAME query key the editor reads
 * (`getGetApplicationQueryKey`), because every one of these routes changes
 * `application_versions.status` and the editor's version list renders it. A
 * publish that did not invalidate would leave the header showing "draft" over
 * a version the catalogue is already serving — the exact class of "the API was
 * right, the screen was wrong" this app has hit before.
 */

interface PublishVariables {
  readonly versionId: number;
  readonly versionName: string;
  readonly category?: PublishCategory | '' | undefined;
  /** From a preceding validation run; lets the server skip re-validating. */
  readonly validationToken?: string | undefined;
}

interface ValidateVariables {
  readonly versionId: number;
  readonly versionName: string;
  readonly category?: PublishCategory | '' | undefined;
}

interface ForkVariables {
  readonly entityId: number;
  readonly targetProjectId: string;
}

export interface EntityLifecycle {
  readonly validate: UseMutationResult<NormalizedPublishValidation, Error, ValidateVariables>;
  readonly publish: UseMutationResult<PublishOutcome, Error, PublishVariables>;
  readonly unpublish: UseMutationResult<void, Error, number>;
  readonly fork: UseMutationResult<ForkOutcome, Error, ForkVariables>;
  readonly exportDocument: (entityId: number) => Promise<EntityExportDocument>;
}

/**
 * Turns an `EliteaApiError` into text a person can act on.
 *
 * The refusals this plane actually produces are specific and each means
 * something different to the author: 403 is the deployment-wide publish
 * guardrail or a missing grant, 409 is "already published", 422 is a
 * validation refusal. Collapsing them to "request failed" would tell an
 * author blocked by an operator's kill switch to fix their agent.
 */
export function lifecycleErrorMessage(error: unknown): string {
  if (!(error instanceof EliteaApiError)) {
    return t('features.agentLifecycle.error.unknown', 'The action could not be completed.');
  }
  const failure = error.failure;
  if (failure.kind === 'auth') {
    return t('features.agentLifecycle.error.auth', 'You are not signed in. Sign in and try again.');
  }
  if (failure.kind === 'network') {
    return t('features.agentLifecycle.error.network', 'The server could not be reached.');
  }
  if (failure.kind === 'aborted') {
    return t('features.agentLifecycle.error.aborted', 'The action was cancelled.');
  }
  const serverMessage = readServerError(failure.body);
  switch (failure.status) {
    case 403:
      return (
        serverMessage ??
        t('features.agentLifecycle.error.forbidden', 'Publishing is not permitted for you on this deployment.')
      );
    case 409:
      return serverMessage ?? t('features.agentLifecycle.error.conflict', 'This version is already published.');
    case 422:
      return serverMessage ?? t('features.agentLifecycle.error.invalid', 'The version did not pass validation.');
    default:
      return serverMessage ?? t('features.agentLifecycle.error.unknown', 'The action could not be completed.');
  }
}

function readServerError(payload: unknown): string | undefined {
  if (typeof payload !== 'object' || payload === null) return undefined;
  const value = (payload as Record<string, unknown>)['error'];
  return typeof value === 'string' && value !== '' ? value : undefined;
}

export function useEntityLifecycle(projectId: string | undefined, entityId: string | undefined): EntityLifecycle {
  const queryClient = useQueryClient();

  const invalidate = useCallback(() => {
    if (projectId === undefined || entityId === undefined) return;
    void queryClient.invalidateQueries({ queryKey: getGetApplicationQueryKey(projectId, Number(entityId)) });
  }, [queryClient, projectId, entityId]);

  const validate = useMutation<NormalizedPublishValidation, Error, ValidateVariables>({
    mutationFn: async (variables) =>
      normalizePublishValidation(
        await runPublishValidation(projectId ?? '', variables.versionId, variables.versionName, variables.category),
      ),
  });

  const publish = useMutation<PublishOutcome, Error, PublishVariables>({
    mutationFn: (variables) =>
      publishVersion(projectId ?? '', variables.versionId, {
        version_name: variables.versionName,
        ...(variables.category === undefined || variables.category === '' ? {} : { category: variables.category }),
        ...(variables.validationToken === undefined ? {} : { validation_token: variables.validationToken }),
      }),
    onSuccess: invalidate,
  });

  const unpublish = useMutation<void, Error, number>({
    mutationFn: (versionId) => unpublishVersion(projectId ?? '', versionId),
    onSuccess: invalidate,
  });

  const fork = useMutation<ForkOutcome, Error, ForkVariables>({
    // Two calls, deliberately: the fork document must be READ from the source
    // project with `?fork=true` before it is written to the target. The server
    // fork handler reads only the body, so a fork built from anything else
    // loses the parent pointers and every version variable.
    mutationFn: async (variables) =>
      forkEntity(variables.targetProjectId, await fetchForkDocument(projectId ?? '', variables.entityId)),
  });

  const exportDocument = useCallback(
    (targetEntityId: number) => fetchExportDocument(projectId ?? '', targetEntityId),
    [projectId],
  );

  return useMemo(
    () => ({ validate, publish, unpublish, fork, exportDocument }),
    [validate, publish, unpublish, fork, exportDocument],
  );
}

export interface EntityImport {
  readonly run: UseMutationResult<ImportOutcome, Error, EntityExportDocument>;
}

/**
 * Import is a LIST-page action, not an editor one, so it owns its own hook and
 * invalidates the list rather than one entity.
 *
 * The invalidation is a prefix match on the applications list key. The import
 * creates a new agent, and the tab that triggered the import is showing the
 * list it must appear in — a mutation that answered 201 and left the list
 * unchanged reads to the user as an import that did nothing.
 */
export function useEntityImport(projectId: string | undefined): EntityImport {
  const queryClient = useQueryClient();
  const run = useMutation<ImportOutcome, Error, EntityExportDocument>({
    mutationFn: (document) => importEntities(projectId ?? '', document),
    onSuccess: () => {
      void queryClient.invalidateQueries({
        predicate: (query) =>
          typeof query.queryKey[0] === 'string' &&
          query.queryKey[0].startsWith(`/elitea_core/applications/prompt_lib/${projectId ?? ''}`),
      });
    },
  });
  return { run };
}
