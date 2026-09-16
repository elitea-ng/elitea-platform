import { useCallback, useState } from 'react';

import { useQueryClient } from '@tanstack/react-query';

import { getSaveApplicationNewVersionQueryOptions } from '@/shared/api/generated/applications/applications';
import type {
  ApplicationVersionDetail,
  SaveApplicationNewVersionBody,
  VersionWriteRequest,
} from '@/shared/api/generated/model';

import { applicationErrorMessage } from '../lib/errorMessage';

/**
 * Port of `apps/elitea-ui/src/hooks/application/useSaveNewVersion.js`.
 *
 * `POST /elitea_core/versions/prompt_lib/{projectId}/{applicationId}` uses
 * Main's shared CreateVersion handler. The name is required. An optional
 * sourceVersionId copies the source's exact skill bindings in the same
 * transaction as the new version. Main enforces application and project
 * ownership; the source ID is not part of the saved version metadata.
 * See the internal-elitea-mcp source mapping for the shared REST/MCP contract.
 *
 * **Same redesign posture as `useCreateApplication.ts`/`useSaveVersion.ts`:**
 * no Formik, no pipeline-editor coupling (caller resolves `instructions` to
 * compiled YAML before calling this hook — see `useCreateApplication.ts`
 * point 3), no navigation/nav-blocker (see `useCreateApplication.ts` point
 * 4).
 *
 * **`onSaveTools` gates `onSuccess` only, not the POST — matching
 * `useSaveNewVersion.js:112-149` exactly, not `useSaveVersion.js`'s
 * position.** The baseline fires `saveNewVersion(...)` (the version-create
 * mutation) UNCONDITIONALLY, before even checking `onSaveTools`; only when
 * `onSaveTools` resolves `false` does it skip `onSuccessHandler` (the
 * navigation trigger) — the new version is always created on the backend
 * regardless. `onSuccess` here is this hook's stand-in for that navigation
 * trigger (see the "no navigation" point above), so it is gated the same
 * way; the created version itself is always returned once the POST
 * succeeds, since (unlike the baseline, which also has a separate
 * `isSavingNewVersionSuccess`-driven toast independent of this function's
 * return value) this hook's return value is the only channel a caller has
 * for "the version was created" — withholding it on a tool-save hiccup
 * would hide a real, persisted creation. See `useSaveChangedTools.ts` for
 * why that gate cannot actually persist a `selected_tools` change today
 * (and therefore never actually resolves `false` in practice).
 */

export interface SaveNewVersionInput {
  readonly projectId: string;
  readonly applicationId: number;
  readonly name: string;
  readonly sourceVersionId?: number;
  /** Every `VersionWriteRequest` field except `name` (supplied separately, see module doc — required on this operation only). */
  readonly version: Omit<VersionWriteRequest, 'name'>;
}

export interface UseSaveNewVersionOptions {
  readonly onSaveTools?: () => Promise<boolean>;
  readonly onSuccess?: (data: ApplicationVersionDetail) => void;
}

export interface UseSaveNewVersionResult {
  readonly onCreateNewVersion: (input: SaveNewVersionInput) => Promise<ApplicationVersionDetail | undefined>;
  readonly isSavingNewVersion: boolean;
  readonly error: unknown;
  readonly errorMessage: string | undefined;
}

export function useSaveNewVersion(options: UseSaveNewVersionOptions = {}): UseSaveNewVersionResult {
  const { onSaveTools, onSuccess } = options;
  const queryClient = useQueryClient();
  const [isSavingNewVersion, setIsSavingNewVersion] = useState(false);
  const [error, setError] = useState<unknown>(undefined);

  const onCreateNewVersion = useCallback(
    async (input: SaveNewVersionInput): Promise<ApplicationVersionDetail | undefined> => {
      setIsSavingNewVersion(true);
      setError(undefined);
      try {
        const body: SaveApplicationNewVersionBody = {
          ...input.version,
          name: input.name,
          ...(input.sourceVersionId === undefined ? {} : { copy_skills_from_version_id: input.sourceVersionId }),
        };
        const queryOptions = getSaveApplicationNewVersionQueryOptions(input.projectId, input.applicationId, body);
        const response = await queryClient.query(queryOptions);
        const created = (response as { data: ApplicationVersionDetail }).data;

        // Matches `useSaveNewVersion.js:144-147`: the version is already
        // created above regardless of this gate; only the success/
        // navigation trigger is conditional on it.
        if (onSaveTools !== undefined && !(await onSaveTools())) {
          return created;
        }

        onSuccess?.(created);
        return created;
      } catch (caught) {
        setError(caught);
        return undefined;
      } finally {
        setIsSavingNewVersion(false);
      }
    },
    [onSaveTools, onSuccess, queryClient],
  );

  return {
    onCreateNewVersion,
    isSavingNewVersion,
    error,
    errorMessage: error === undefined ? undefined : applicationErrorMessage(error),
  };
}
