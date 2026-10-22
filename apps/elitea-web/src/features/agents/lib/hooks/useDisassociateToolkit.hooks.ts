import { useCallback, useRef, useState } from 'react';

import { useQueryClient } from '@tanstack/react-query';

import { getUpdateApplicationRelationQueryOptions } from '@/shared/api/generated/applications/applications';
import { t } from '@/shared/i18n';

import { useSelectedProjectId } from '../../api/useSelectedProjectId';
import { resolveToolkitId, setToolkitRelation } from '../toolRelation';
import type { AgentToolAssociation } from '../types';
import { useSetRefetchDetails } from './useRefetchAgentDetails.hooks';

/**
 * Ported from
 * `apps/elitea-ui/src/[fsd]/features/agent/lib/hooks/useDisassociateToolkit.hooks.js`.
 *
 * **DEVIATIONS FROM BASELINE (all disclosed):**
 *
 *  1. `useToolkitAssociateMutation` (`@/api/toolkits`, `{projectId,
 *     toolkitId, entity_version_id, entity_id, entity_type: 'agent',
 *     has_relation: false}` relation-TOGGLE call) -> `../toolRelation.ts`'s
 *     `setToolkitRelation({..., hasRelation: false})`, which is that same
 *     `PATCH /elitea_core/tool/prompt_lib/{project_id}/{toolkit_id}` call —
 *     the one the already-landed ATTACH path (`ui/ToolMenu.tsx`) uses with
 *     `hasRelation: true`, now shared by both directions so they cannot
 *     drift apart. No cross-feature toolkit-domain hook is needed for it
 *     (no orval wrapper exists either; see `toolRelation.ts`).
 *
 *     **This replaces an earlier port of this branch that called the
 *     generated `useDeleteApplicationTool` (`DELETE /elitea_core/tool/
 *     prompt_lib/{projectId}/{toolId}`) with `tool.id`. That was wrong in
 *     two independent ways, both verified by reading the Go source rather
 *     than the endpoint's name:**
 *       - That route is wired to `toolkits.Handler.Delete` ->
 *         `pgRepo.DeleteToolkit` -> `DELETE FROM p_{id}.elitea_tools WHERE
 *         id = $1` (`router.go:984`, `toolkits/handler.go:631,1067`). It
 *         deletes the toolkit INSTANCE itself, project-wide, and
 *         `entity_tool_mapping.tool_id` is `REFERENCES elitea_tools(id) ON
 *         DELETE CASCADE` (`001_initial.sql:455`) — so it also detaches
 *         that toolkit from every OTHER agent in the project. Detaching one
 *         tool from one version is not what it does.
 *       - `tool.id` is `entity_tool_mapping.id` (the mapping row's own
 *         serial), while that route's path parameter is `elitea_tools.id`.
 *         Both are `SERIAL` in the same schema, so the call does not fail
 *         loudly — it deletes whichever unrelated toolkit instance happens
 *         to carry that serial.
 *     The relation PATCH is keyed on `elitea_tools.id` too, so the id sent
 *     is `resolveToolkitId(tool)` (`tool.tool_id`), NOT `tool.id`. The
 *     baseline sent `tool.id` here, which was correct against the legacy
 *     runtime's `tools[]` shape and is not against this backend's.
 *  2. `useUpdateApplicationRelationMutation` (`@/api/applications`) -> the
 *     real generated `useUpdateApplicationRelation`, same tag, real
 *     endpoint, no deviation beyond the RTK-Query-to-TanStack-Query call
 *     shape (`queryClient.query(getUpdateApplicationRelationQueryOptions(...))`,
 *     the established imperative-trigger convention for this generated
 *     client's `useQuery`-shaped write endpoints — see
 *     `entities/application-form/model/mutations.ts`'s own doc comment for
 *     why every POST/PUT/DELETE here is `useQuery`-shaped, not
 *     `useMutation`).
 *  3. `usePipelineToolsChanges` (`@/hooks/pipeline/usePipelineToolsChanges`)
 *     — a REAL `features/pipelines` (A2) dependency, forbidden by
 *     `no-sideways-features` (absolute, no carve-out — this batch's
 *     preamble names this exact hook as the reason A1a's `useSavePipeline`
 *     needed the same treatment). Its four members are split by whether
 *     they are pipeline-SPECIFIC or genuinely shared:
 *       - `onRemoveTool` is called UNCONDITIONALLY on every removal
 *         (agent AND pipeline call sites both use this hook via the shared
 *         `ToolCard`), so it is an OPTIONAL injected callback
 *         (`onToolRemovedFromFlow`), not gated behind `isFromPipeline`.
 *       - `isFromPipeline`, `getCleanYamlForInitial`,
 *         `syncInitStateWithCleanYaml` are used ONLY inside the pipeline
 *         auto-save branch (`savePipelineAfterToolkitRemoval`) — folded
 *         into one OPTIONAL `onPipelineAutoSave` callback, invoked only
 *         when the caller passes `isFromPipeline: true`. A page/widget
 *         composing both `features/agents` and `features/pipelines`
 *         supplies the real implementation; this hook does not know pipeline
 *         YAML internals at all.
 *  4. `useFormikContext` (`resetForm`/`setValues`/`values`/`initialValues`/
 *     `dirty`) -> no Formik in this app (confirmed absent from
 *     `node_modules`) — explicit `FormState`/`FormActions` parameters,
 *     matching this slice's established convention (see
 *     `useSaveAgentToolVariables.ts`'s own "DEVIATION FROM BASELINE" doc
 *     comment). `resetForm({values: {...initialValues, version_details:
 *     updated}})` + `setValues({...values, version_details: updated})`
 *     (Formik's "rebase the Discard baseline AND the current values at
 *     once") becomes one `onToolRemoved({tools, initialTools})` callback —
 *     the caller applies both halves however its own form library expects
 *     (e.g. react-hook-form's `reset(newDefaults, {keepDirtyValues:
 *     true})` plus a `setValue` for the current tools).
 *  5. `useDispatch`/`eliteaApi.util.invalidateTags([TAG_TYPE_APPLICATION_DETAILS])`
 *     -> `queryClient.invalidateQueries` is NOT called here: this hook has
 *     no reliable single query key for "this application's detail" without
 *     re-deriving `getGetApplicationQueryKey` itself, and the ONLY call
 *     site that needs it (`isStaleVersionReferenceError`'s recovery path)
 *     already calls `setRefetch()` (deviation 6) immediately after, which
 *     is this slice's own, already-landed mechanism for exactly this
 *     "avoid a real refetch fighting the form's local edits" situation.
 *     The SAME recovery path also drops the baseline's
 *     `toastInfo('Tool reference was outdated. Page has been refreshed
 *     with current state.')` — no toast infrastructure exists in this app
 *     yet (same precedent as `features/mcps/model/useMcpAuthCheck.ts`'s
 *     "`useToast` is replaced with an `onError` callback", and
 *     `api/useAgentPipelineAssociation.tsx`'s own gap-3 doc comment).
 *     Restored as an optional injected `onStaleVersionReference?: (message:
 *     string) => void` param (`DisassociateToolkitFormActions`), called
 *     with the same i18n'd message text right where `setRefetch()` already
 *     fires — the caller decides how (or whether) to surface it, same as
 *     every other optional callback this hook already exposes
 *     (`onToolRemovedFromFlow`/`onPipelineAutoSave`).
 *  6. `useSetRefetchDetails` (`@/[fsd]/features/agent/lib/hooks/
 *     useRefetchAgentDetails.hooks`) -> this sub-unit's own already-ported
 *     `useRefetchAgentDetails.hooks.ts` (intra-slice, no deviation).
 *  7. `useSelector(state => state.user)` (`{id: currentUserId}`, used only
 *     by `clearTools`/`savePipelineAfterToolkitRemoval`'s pipeline-save
 *     path) — folded into `onPipelineAutoSave`'s responsibility (deviation
 *     3); this hook no longer needs the current user id at all once the
 *     pipeline save itself is the caller's job.
 *  8. `clearTools` (`@/common/applicationUtils`) — pipeline-save-only
 *     helper (`clearTools(tools, currentUserId)`, strips per-user transient
 *     fields before an auto-save); moved inside `onPipelineAutoSave`'s
 *     responsibility for the same reason as deviation 7.
 *  9. `handleApplicationRelationRemoval`'s baseline
 *     (`useDisassociateToolkit.hooks.js:189-223`) has NO precondition guard
 *     on `applicationId`/`versionId`/`projectId`/`tool.settings?.
 *     application_id`/`application_version_id` — it always calls
 *     `updateApplicationRelation(...)` and lets a request built from
 *     missing ids fail server-side, landing in its own `catch` ->
 *     `toastError(...)` branch. The generated
 *     `getUpdateApplicationRelationQueryOptions` requires concrete
 *     `string`/`number` arguments (no `undefined` overload), so a
 *     precondition check is unavoidable here for type-safety — but it must
 *     produce the SAME user-visible outcome as the baseline (a surfaced
 *     failure), not silently return. It therefore sets
 *     `isDisassociateError`/`disassociateError` directly — the same error
 *     state the `catch` branch below sets — instead of returning with no
 *     signal at all.
 */

export interface DisassociateToolkitFormState {
  /** `values.version_details.tools` — the caller's current controlled tools array. */
  readonly tools: readonly AgentToolAssociation[];
  /** `initialValues.version_details.tools` — the caller's "Discard" baseline. */
  readonly initialTools: readonly AgentToolAssociation[];
  /** `values.dirty` — whether the form has any unsaved changes right now (before this removal). */
  readonly dirty: boolean;
}

export interface ToolRemovalUpdate {
  readonly tools: readonly AgentToolAssociation[];
  readonly initialTools: readonly AgentToolAssociation[];
}

export interface DisassociateToolkitFormActions {
  /** Applies both the new current `tools[]` and the new Discard-baseline `tools[]` after a successful removal. */
  readonly onToolRemoved: (update: ToolRemovalUpdate) => void;
  /** See module doc comment, deviation 3. Called unconditionally on every successful removal. */
  readonly onToolRemovedFromFlow?: (tool: AgentToolAssociation) => void;
  /** See module doc comment, deviation 3. Only invoked when `isFromPipeline` is true. */
  readonly onPipelineAutoSave?: (params: {
    readonly tool: AgentToolAssociation;
    readonly updatedInitialTools: readonly AgentToolAssociation[];
    readonly isAttachmentToolkit: boolean;
  }) => void | Promise<void>;
  readonly isFromPipeline?: boolean;
  /** See module doc comment, deviation 5 — baseline's `toastInfo(...)` on the stale-version-reference recovery path. Called with an already-i18n'd message, right where `setRefetch()` fires. */
  readonly onStaleVersionReference?: (message: string) => void;
}

export interface UseDisassociateToolkitParams extends DisassociateToolkitFormState, DisassociateToolkitFormActions {
  readonly applicationId: number | undefined;
  readonly versionId: number | undefined;
  readonly onDeleteAttachmentTool?: () => void;
  readonly index: number;
}

export interface DisassociateToolArgs {
  readonly tool: AgentToolAssociation;
  readonly isAttachmentToolkit?: boolean;
}

export interface UseDisassociateToolkitResult {
  readonly onDisassociateTool: (args: DisassociateToolArgs) => Promise<void>;
  readonly isLoading: boolean;
  readonly isDisassociateError: boolean;
  readonly disassociateError: unknown;
}

/**
 * `useDisassociateToolkit.hooks.js:26-29`. The baseline reads
 * `error?.data?.error || error?.message` — an RTK-Query-shaped error whose
 * `.message` was itself the readable server text. This app's transport
 * (`shared/api/generated/mutator.ts`'s `EliteaApiError`) deliberately does
 * NOT put the server's response body in `.message` (`describeFailure`
 * synthesizes a generic `"eliteaFetch: 400 from <url>"` string instead,
 * verified by reading `mutator.ts` directly) — the real body survives on
 * `error.failure.body` (`HttpFailure`'s `{kind: 'http', ..., body:
 * unknown}` variant, `shared/api/http.ts:47`). Checks the real body's
 * `.error` string first (this endpoint's documented shape,
 * `applicationRelationUpdatedResponse.zod.ts`'s sibling error envelope:
 * `{"error": "message"}`), falling back to `.message` for any other
 * `Error`-shaped rejection (defensive — this hook should never crash trying
 * to classify an error it doesn't recognise).
 */
function isStaleVersionReferenceError(error: unknown): boolean {
  const failureBody =
    typeof error === 'object' && error !== null && 'failure' in error
      ? (error as { failure?: { body?: unknown } }).failure?.body
      : undefined;
  const rawBodyError =
    typeof failureBody === 'object' && failureBody !== null && 'error' in failureBody
      ? (failureBody as { error?: unknown }).error
      : undefined;
  const bodyErrorText = typeof rawBodyError === 'string' ? rawBodyError : '';
  const message = error instanceof Error ? error.message : '';
  return bodyErrorText.includes('Already removed relation') || message.includes('Already removed relation');
}

/**
 * `useDisassociateToolkit.hooks.js:141-188`'s `applyToolRemoval`, narrowed
 * to just the `tools[]` array transform. **Real, disclosed scope
 * narrowing:** the baseline's `isAttachmentToolkit` branch ALSO clears
 * `version_details.meta.attachment_toolkit_id` — VERSION-level meta this
 * hook has no access to (it only receives `tools[]`/`initialTools[]`, per
 * the module doc comment's deviation 4, not the whole `version_details`
 * object). That clearing is left to the caller, which already receives an
 * unconditional `onDeleteAttachmentTool` signal for exactly this case (the
 * baseline's own `onDeleteAttachmentTool?.()` call, preserved below) —
 * the caller clears `meta.attachment_toolkit_id` itself when that fires.
 */
function applyToolRemoval(
  state: DisassociateToolkitFormState,
  tool: AgentToolAssociation,
  toolIndex: number,
): ToolRemovalUpdate {
  return {
    tools: state.tools.filter((_, i) => i !== toolIndex),
    initialTools: state.initialTools.filter((t) => t.id !== tool.id),
  };
}

export function useDisassociateToolkit(params: UseDisassociateToolkitParams): UseDisassociateToolkitResult {
  const { applicationId, versionId, onDeleteAttachmentTool } = params;
  const projectId = useSelectedProjectId();
  const { setRefetch } = useSetRefetchDetails();
  const queryClient = useQueryClient();

  const [isLoading, setIsLoading] = useState(false);
  const [isDisassociateError, setIsDisassociateError] = useState(false);
  const [disassociateError, setDisassociateError] = useState<unknown>(undefined);

  // Always-latest ref for the tools/form-state/callback fields `commitRemoval` needs — keeps that
  // callback's own dependency array under this codebase's §3.5 hook-deps budget (8 max) without
  // sacrificing real memoisation (a plain `[params]` dependency would recreate the callback every
  // render, since the caller passes a fresh `tools`/`initialTools` array each render).
  const paramsRef = useRef(params);
  paramsRef.current = params;

  const reset = useCallback(() => {
    setIsDisassociateError(false);
    setDisassociateError(undefined);
  }, []);

  const commitRemoval = useCallback(
    async (tool: AgentToolAssociation, isAttachmentToolkit: boolean) => {
      const p = paramsRef.current;
      const update = applyToolRemoval({ tools: p.tools, initialTools: p.initialTools, dirty: p.dirty }, tool, p.index);
      p.onToolRemoved(update);
      p.onToolRemovedFromFlow?.(tool);
      if (!p.dirty) {
        setRefetch();
      }
      if (p.isFromPipeline) {
        await p.onPipelineAutoSave?.({ tool, updatedInitialTools: update.initialTools, isAttachmentToolkit });
      }
    },
    [setRefetch],
  );

  const handleApplicationRelationRemoval = useCallback(
    async (tool: AgentToolAssociation) => {
      if (
        applicationId === undefined ||
        versionId === undefined ||
        projectId === undefined ||
        tool.settings?.application_id === undefined ||
        tool.settings.application_version_id === undefined
      ) {
        // See module doc comment, deviation 9: the baseline has no equivalent guard and always
        // makes the call, letting a request built from missing ids fail server-side and surface
        // through the SAME catch branch below. Matching that user-visible outcome (a surfaced
        // failure, not a silent no-op) without an actual type-unsafe call.
        setIsDisassociateError(true);
        setDisassociateError(
          new Error(
            t(
              'features.agents.useDisassociateToolkit.missingRelationReference',
              'Failed to update application relation: missing application or version reference.',
            ),
          ),
        );
        return;
      }
      try {
        setIsLoading(true);
        const options = getUpdateApplicationRelationQueryOptions(
          projectId,
          Number(tool.settings.application_id),
          Number(tool.settings.application_version_id),
          { application_id: applicationId, version_id: versionId, has_relation: false },
        );
        await queryClient.query(options);
        // `commitRemoval` (below) already calls `onToolRemovedFromFlow?.(tool)` — an extra call
        // here double-fired it for every application-type tool disassociation (confirmed live,
        // `mock.calls.length === 2`). Removed; `commitRemoval` is the single source of this call.
        await commitRemoval(tool, false);
        reset();
      } catch (error) {
        if (isStaleVersionReferenceError(error)) {
          setRefetch();
          paramsRef.current.onStaleVersionReference?.(
            t(
              'features.agents.useDisassociateToolkit.staleVersionReference',
              'Tool reference was outdated. Page has been refreshed with current state.',
            ),
          );
        } else {
          setIsDisassociateError(true);
          setDisassociateError(error);
        }
      } finally {
        setIsLoading(false);
      }
    },
    [applicationId, versionId, projectId, queryClient, commitRemoval, reset, setRefetch],
  );

  const onDisassociateTool = useCallback(
    async ({ tool, isAttachmentToolkit = false }: DisassociateToolArgs) => {
      const toolkitId = resolveToolkitId(tool);
      if (applicationId !== undefined && toolkitId !== undefined && versionId !== undefined && projectId !== undefined) {
        if (tool.type !== 'application') {
          try {
            setIsLoading(true);
            await setToolkitRelation({ projectId, applicationId, versionId, toolkitId, hasRelation: false });
            await commitRemoval(tool, isAttachmentToolkit);
            if (isAttachmentToolkit) {
              onDeleteAttachmentTool?.();
            }
            reset();
          } catch (error) {
            setIsDisassociateError(true);
            setDisassociateError(error);
          } finally {
            setIsLoading(false);
          }
        } else {
          await handleApplicationRelationRemoval(tool);
        }
      } else {
        await handleApplicationRelationRemoval(tool);
      }
    },
    [applicationId, versionId, projectId, commitRemoval, onDeleteAttachmentTool, reset, handleApplicationRelationRemoval],
  );

  return { onDisassociateTool, isLoading, isDisassociateError, disassociateError };
}
