import { useCallback, useMemo } from 'react';

import { useMutation, useQueryClient, type UseMutationResult } from '@tanstack/react-query';

import { getGetApplicationQueryKey } from '@/shared/api/generated/applications/applications';
import { getListAgentTeamsQueryKey } from '@/shared/api/generated/chat/chat';
import { EliteaApiError } from '@/shared/api/generated/mutator';
import { serverErrorSentence } from '@/shared/api/serverErrorMessage';
import { t } from '@/shared/i18n';

import {
  fetchExportDocument,
  fetchForkDocument,
  forkEntityWithNested,
  importEntities,
  publishVersion,
  runPublishValidation,
  unpublishVersion,
  type EntityExportDocument,
  type ForkOutcome,
  type ImportOutcome,
  type PublishOutcome,
} from '../api/lifecycleApi';
import { uploadImportMedia } from '../api/importMediaApi';
import { importTeamDocument, type TeamImportTeamBody } from '../api/teamImportApi';
import { attachImportAvatars } from '../lib/importAvatars';
import type { PendingAvatar } from '../lib/importTypes';
import { governanceRefusalMessage, readGovernanceRefusal } from '../lib/governanceRefusal';
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
  /** What changed; the Marketplace listing shows it per version (ADR-0051). */
  readonly releaseNotes?: string | undefined;
}

interface ValidateVariables {
  readonly versionId: number;
  readonly versionName: string;
  readonly category?: PublishCategory | '' | undefined;
}

interface ForkVariables {
  readonly entityId: number;
  /** The version to copy; the newest when absent. */
  readonly versionId?: number | undefined;
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
  if (failure.kind !== 'http') return transportFailureMessage(failure.kind);
  // A governance refusal carries its code in `error` too; read it as a code,
  // never as text, so the author sees a sentence and not `release_gate_failed`.
  const refusal = readGovernanceRefusal(error);
  if (refusal !== undefined) return governanceRefusalMessage(refusal);
  const serverMessage = serverErrorSentence(failure.body);
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

function transportFailureMessage(kind: 'auth' | 'network' | 'aborted'): string {
  switch (kind) {
    case 'auth':
      return t('features.agentLifecycle.error.auth', 'You are not signed in. Sign in and try again.');
    case 'network':
      return t('features.agentLifecycle.error.network', 'The server could not be reached.');
    case 'aborted':
      return t('features.agentLifecycle.error.aborted', 'The action was cancelled.');
  }
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
        ...(variables.releaseNotes === undefined || variables.releaseNotes.trim() === ''
          ? {}
          : { release_notes: variables.releaseNotes.trim() }),
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
      forkEntityWithNested(
        projectId ?? '',
        variables.targetProjectId,
        await fetchForkDocument(projectId ?? '', variables.entityId, variables.versionId),
      ),
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

/** What an Import posts: the document, and the team when the file is a team zip (issue 3010). */
interface EntityImportRequest {
  readonly document: EntityExportDocument;
  readonly team?: TeamImportTeamBody | undefined;
  /** A zip's avatar media (issue 3137), uploaded before the import so the document carries urls. */
  readonly avatars?: readonly PendingAvatar[] | undefined;
}

export interface EntityImport {
  readonly run: UseMutationResult<ImportOutcome, Error, EntityImportRequest>;
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
  const run = useMutation<ImportOutcome, Error, EntityImportRequest>({
    mutationFn: async ({ document, team, avatars }) => {
      const project = projectId ?? '';
      const withAvatars = await attachImportAvatars(document, avatars ?? [], (file) => uploadImportMedia(project, file));
      return team === undefined ? importEntities(project, withAvatars) : importTeamDocument(project, withAvatars, team);
    },
    onSuccess: (_outcome, { team }) => {
      void queryClient.invalidateQueries({
        predicate: (query) =>
          typeof query.queryKey[0] === 'string' &&
          query.queryKey[0].startsWith(`/elitea_core/applications/prompt_lib/${projectId ?? ''}`),
      });
      // A team import also adds a team to the Teams tab.
      if (team !== undefined) void queryClient.invalidateQueries({ queryKey: getListAgentTeamsQueryKey(projectId ?? '') });
    },
  });
  return { run };
}
