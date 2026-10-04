import { useCallback, useEffect, useState } from 'react';

import { useQueryClient } from '@tanstack/react-query';

import { getEditApplicationQueryOptions, getUpdateApplicationVersionQueryOptions } from '@/shared/api/generated/applications/applications';
import type {
  ApplicationUpdateRequest,
  ApplicationUpdatedResponse,
  ApplicationVersionDetail,
  VersionWriteRequest,
} from '@/shared/api/generated/model';

import { applicationErrorMessage } from '../lib/errorMessage';

import { useApplicationsStore } from './applicationsStore';

/**
 * Port of `apps/elitea-ui/src/hooks/application/useSaveVersion.js`.
 *
 * **DISCLOSED REDESIGN — the baseline's ONE combined PUT is genuinely TWO
 * endpoints on the real backend, traced to Go source, not assumed:**
 *
 * The baseline calls `useApplicationEditMutation()` (`applicationEdit`,
 * `PUT /application/prompt_lib/{projectId}/{id}`) with a body carrying BOTH
 * application-level fields (`name`, `description`, `owner_id`,
 * `webhook_secret`) AND a nested `version: {...version_details, llm_settings,
 * tags, tools, conversation_starters, instructions, pipeline_settings, meta}`.
 * Reading `services/elitea-main/internal/api/v2/applications/handler.go`'s
 * `Update` handler (:486-613) directly shows its nested-version branch reads
 * ONLY `version.name` (guarded against renaming `base`/`latest`) and
 * `version.instructions` — nothing else; `owner_id`/`webhook_secret` are not
 * read at the application level either (`ApplicationUpdateRequest` —
 * `applicationUpdateRequest.zod.ts` — confirms: `name`/`description`/`icon`/
 * a thin `version` stub, nothing more). The RICH version fields the baseline
 * actually needs to persist (`llm_settings`, `welcome_message`,
 * `conversation_starters`, `meta`, `agent_type`) are read by a
 * DIFFERENT handler, `UpdateVersion` (:808-930, routed at
 * `PUT /version/prompt_lib/{projectId}/{applicationId}/{versionId}`,
 * `useUpdateApplicationVersion`/`VersionWriteRequest`) — confirmed field-by-
 * field against that handler's SQL `setClauses` construction.
 *
 * **`variables` used to be a backend-side no-op on UPDATE. It no longer
 * is (#307).** `UpdateVersion` now has a `variables` branch and replaces
 * the version's `application_variables` rows, so what this hook sends is
 * what the editor reads back.
 *
 * So this hook issues the two real calls the one baseline call used to
 * fake: `updateApplicationVersion` for the version's mutable fields, and
 * (only when `applicationName`/`applicationDescription`/`applicationIcon`
 * are supplied) `editApplication` for the application shell — WITHOUT that
 * second call's own thin `version` stub (which would only redundantly echo
 * `name`/`instructions` through a narrower path, and risks the "cannot
 * rename base/latest version" 400 guard for no benefit once
 * `updateApplicationVersion` already carries the real version update).
 *
 * **Real, disclosed gaps, not invented fields:**
 * - `tags` USED to have no field on `VersionWriteRequest` at all, which is
 *   why the agent editor's tag control could not be mounted. Closed (#345):
 *   the field exists, `UpdateVersion` writes the
 *   `application_version_tag_association` rows, and both read paths return
 *   them. A caller sends the whole list; an absent key leaves the stored
 *   set alone and an empty array clears it.
 *   `entities/application-form/model/mutations.ts`'s
 *   `useSaveApplicationVersion` still discloses the old gap for its own
 *   narrower call — that file is outside this change's scope.
 * - `webhook_secret` has no field on `ApplicationUpdateRequest`, and it is
 *   not sent. It is not a gap any more: legacy issue 6656 moved the agent's
 *   webhook to the inbound trigger, which the editor's Triggers section
 *   (`pages/agents/ui/EditApplicationTriggersPanel.tsx`) creates and rotates
 *   on its own routes. The server mints that credential; no save carries it.
 * - `pipeline_settings` DOES have a write field now (`VersionWriteRequest`,
 *   added for #135) but is still not sent from this agents-domain hook: it
 *   has no live flow-editor state to read. The pipelines domain's own save
 *   path (`pages/pipelines/lib/useEditPipelineForm.ts` +
 *   `features/pipelines`' `usePipelineGraphDraft`) is where a pipeline graph
 *   is written; an agent version has no graph. Omitting the key leaves the
 *   stored column untouched, so this hook cannot blank one.
 * - Tool changes (`selected_tools`) go through `useSaveChangedTools`'s
 *   `onSaveTools` gate FIRST, exactly like the baseline's `onSave`
 *   (`useSaveVersion.js:57-60`) gates on `onSaveTools()` before its own
 *   save call — see that hook's own doc comment for why it cannot actually
 *   persist a `selected_tools` change today, and why it therefore always
 *   resolves `true` rather than blocking this PUT the way a genuine
 *   baseline failure would.
 * - The baseline also fetches `useListModelsQuery` and runs
 *   `cleanLLMSettings(llmSettings, model)` to strip fields (e.g.
 *   `reasoning_effort`) the selected model doesn't support, before sending
 *   `llm_settings`. That cleaning now happens where the model is chosen
 *   rather than here: `shared/api/agentLlmSettings.ts` models the closed set
 *   of keys the worker accepts, and the picker writes at most one of
 *   `temperature`/`reasoning_effort` because the worker refuses both
 *   together. `input.version.llm_settings` is still sent to
 *   `updateApplicationVersion` verbatim, so a caller that builds the object
 *   by hand rather than through that module owns its own validity.
 *   (An earlier revision of this note said no `ListModels`-shaped endpoint
 *   existed. It does — `useListModelsQuery` in
 *   `shared/api/configurationsApi.ts`, hand-written rather than generated,
 *   which is why a grep confined to `shared/api/generated/` came back
 *   empty.)
 *
 * **No Formik, no Redux `state.user`/`eliteaApi.util.updateQueryData` cache
 * patch, no nav-blocker/toast/navigation** — same "plain typed input,
 * caller owns orchestration" redesign as `useCreateApplication.ts`; see that
 * file's doc comment points 1 and 4 for the precedent this follows. Any
 * caller that wants the version-detail query cache to reflect the save
 * immediately can call `queryClient.invalidateQueries` itself using
 * `getGetApplicationVersionDetailQueryOptions(...).queryKey` — this hook
 * does not reach into a cache key it doesn't own conventions for.
 */

export interface SaveVersionInput {
  readonly projectId: string;
  readonly applicationId: number;
  readonly versionId: number;
  /** Version fields — mirrors `VersionWriteRequest` exactly (see the module doc for `pipeline_settings`, the one field this agents-domain hook has no state to send). */
  readonly version: VersionWriteRequest;
  /** Application-shell fields — omit any of these to leave that field alone (no `editApplication` call fires at all when all three are omitted). */
  readonly applicationName?: string;
  readonly applicationDescription?: string;
  readonly applicationIcon?: string;
}

export interface SaveVersionResult {
  readonly versionDetail: ApplicationVersionDetail;
  readonly application?: ApplicationUpdatedResponse;
}

export interface UseSaveVersionResult {
  readonly onSave: (input: SaveVersionInput) => Promise<SaveVersionResult | undefined>;
  readonly isSaving: boolean;
  readonly error: unknown;
  readonly errorMessage: string | undefined;
}

function needsApplicationUpdate(input: SaveVersionInput): boolean {
  return input.applicationName !== undefined || input.applicationDescription !== undefined || input.applicationIcon !== undefined;
}

/**
 * @param onSaveTools Injected gate, matching `useSaveChangedTools`'s
 * `onSaveTools` shape exactly — pass `result.onSaveTools` from a
 * `useSaveChangedTools(...)` call made with the SAME version's current vs.
 * original `tools[]`. Omit to skip the gate entirely (e.g. a caller that
 * has no tools UI on this screen).
 */
export function useSaveVersion(onSaveTools?: () => Promise<boolean>): UseSaveVersionResult {
  const queryClient = useQueryClient();
  const setIsSaving = useApplicationsStore((state) => state.setIsSaving);
  const [isSaving, setIsSavingLocal] = useState(false);
  const [error, setError] = useState<unknown>(undefined);

  useEffect(() => {
    setIsSaving(isSaving);
  }, [isSaving, setIsSaving]);

  const onSave = useCallback(
    async (input: SaveVersionInput): Promise<SaveVersionResult | undefined> => {
      if (onSaveTools !== undefined && !(await onSaveTools())) {
        return undefined;
      }

      setIsSavingLocal(true);
      setError(undefined);
      try {
        const versionOptions = getUpdateApplicationVersionQueryOptions(
          input.projectId,
          input.applicationId,
          input.versionId,
          input.version,
        );
        const versionResponse = await queryClient.query(versionOptions);
        const versionDetail = (versionResponse as { data: ApplicationVersionDetail }).data;

        if (!needsApplicationUpdate(input)) {
          return { versionDetail };
        }

        const body: ApplicationUpdateRequest = {
          ...(input.applicationName !== undefined ? { name: input.applicationName } : {}),
          ...(input.applicationDescription !== undefined ? { description: input.applicationDescription } : {}),
          ...(input.applicationIcon !== undefined ? { icon: input.applicationIcon } : {}),
        };
        const appOptions = getEditApplicationQueryOptions(input.projectId, input.applicationId, body);
        const appResponse = await queryClient.query(appOptions);
        const application = (appResponse as { data: ApplicationUpdatedResponse }).data;

        return { versionDetail, application };
      } catch (caught) {
        setError(caught);
        return undefined;
      } finally {
        setIsSavingLocal(false);
      }
    },
    [onSaveTools, queryClient],
  );

  return {
    onSave,
    isSaving,
    error,
    errorMessage: error === undefined ? undefined : applicationErrorMessage(error),
  };
}
