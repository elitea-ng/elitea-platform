import { useCallback, useEffect, useMemo, useState } from 'react';

import { useNavigate } from '@tanstack/react-router';
import { useQueryClient } from '@tanstack/react-query';
import { useWatch, type Control } from 'react-hook-form';

import type { ApplicationCreationInput } from '@/entities/application-form';
import type { PipelineGraphDraft } from '@/features/pipelines';
import { getGetApplicationQueryKey } from '@/shared/api/generated/applications/applications';
import { t } from '@/shared/i18n';
import { disarmUnsavedChangesNavBlocker } from '@/widgets/app-shell';
import type {
  ApplicationVersionDetail,
  ApplicationVersionSummary,
  VersionWriteRequest,
} from '@/shared/api/generated/model';

import { carryPipelineGraphToVersion } from './carryPipelineGraphToVersion';
import { toNewPipelineVersionBody, toVersionOptions, type EditPipelineVersionOption } from './editPipelineMappers';
import type { EditPipelineVersionFields } from './useEditPipelineVersionFields';

export interface PipelineVersionControlsArgs {
  readonly projectId: string | undefined;
  readonly applicationId: number | undefined;
  readonly tab: string | undefined;
  readonly versions: readonly ApplicationVersionSummary[];
  readonly activeVersion: ApplicationVersionDetail | undefined;
  /**
   * The page's RHF control. Conversation starters are read LIVE off it
   * (`useWatch`) rather than off `activeVersion`, for the reason
   * `pages/agents/lib/useEditApplicationVersionControls.ts` states for its
   * twin: a save-as-version clones the CURRENTLY EDITED version, so starters
   * typed but not yet saved must travel with it.
   */
  readonly control: Control<ApplicationCreationInput>;
  /**
   * The live version-level edits (`useEditPipelineVersionFields`), which win
   * over the version's stored copy. Taking a version used to clone the
   * server's welcome message, model, modules, step limit and variables, so an
   * author who edited the form and then reached for "Save As Version" got a
   * version without the edit they had just made.
   */
  readonly versionFields: EditPipelineVersionFields;
  /** `usePipelineGraphDraft`'s reader — called at click time, never during render. */
  readonly readGraphDraft: () => PipelineGraphDraft | undefined;
  /** Public-project viewer: the selector stays, the write affordances go (`ApplicationTabBar.jsx:65`). */
  readonly isReadOnly: boolean;
  /**
   * The FIRST load only — there is neither a version list nor an active
   * version to show yet.
   *
   * Not "a request is in flight": TanStack keeps the previous detail while it
   * revalidates, and the page refetches after every save
   * (`useRefetchPipelineAfterSave`), so a raw `isFetching` here took the whole
   * version bar off screen each time an author saved. See the call site in
   * `pages/pipelines/EditPipeline.tsx`.
   */
  readonly isFetching: boolean;
  /**
   * Whether the live graph is one the native runtime would accept.
   *
   * "Save As Version" is a SECOND write path onto the same document, and it
   * used to be gated only on `!isReadOnly && activeVersion !== undefined`. It
   * therefore persisted exactly the graph the Save veto
   * (`features/pipelines`' `GraphAdmissionGate`) had just refused: the POST
   * mints the version, `carryPipelineGraphToVersion` PUTs the live graph onto
   * it, and `goToVersion` then OPENS it — so the inadmissible document is
   * both stored and the one the editor shows, and the runtime refuses it with
   * `graph.pipeline.invalid_configuration` at first run.
   */
  readonly isGraphAdmissible: boolean;
}

export interface PipelineVersionControlsState {
  readonly showVersionControls: boolean;
  /** The pipeline id in the string form `SaveNewVersionButton` takes. `''` only while `showVersionControls` is `false`. */
  readonly applicationIdText: string;
  readonly versionOptions: readonly EditPipelineVersionOption[];
  readonly activeVersionId: number | undefined;
  readonly versionBody: Omit<VersionWriteRequest, 'name'>;
  readonly canSaveNewVersion: boolean;
  /**
   * Withholds "Save As Version" alone — NOT the selector, "Set as default" or
   * "Delete version", none of which write the graph. Folding this into
   * `canSaveNewVersion` would have taken all three away from a user whose
   * canvas is mid-edit, including the delete that is sometimes the only way
   * out.
   */
  readonly isSaveNewVersionBlocked: boolean;
  readonly handleSelectVersion: (version: EditPipelineVersionOption) => void;
  readonly handleNewVersionSaved: (created: ApplicationVersionDetail) => void;
  readonly versionDelete:
    | {
        readonly onVersionDeleted: () => void;
        readonly onVersionDeleteError: (message: string) => void;
      }
    | undefined;
  /** The most recent version-write failure, already resolved to a message; `undefined` once a later attempt succeeds. */
  readonly versionError: string | undefined;
  readonly reportVersionError: (message: string) => void;
}

/** `conversation_starters` is a loose array on the form input; the write body takes `string[]`. */
function isString(entry: unknown): entry is string {
  return typeof entry === 'string';
}

/** Stable empty body for the window before the active version resolves — a fresh literal each render would make `versionBody` a new prop reference every time. */
const EMPTY_VERSION_BODY: Omit<VersionWriteRequest, 'name'> = {};

/**
 * The page-side half of the pipeline editor's version bar: everything
 * `features/agents`' deliberately-dumb `AgentVersionControls` refuses to own
 * — which route a version switch navigates to, what body a "Save As Version"
 * clones, and what happens once one exists.
 *
 * **The version bar itself is REUSED, not re-implemented.** A Pipeline is an
 * Application row, its versions are `application_versions` rows, and every
 * route the bar drives (`POST /versions/...`, `PATCH /default_version/...`,
 * `DELETE /version/...`) is application-scoped and already generated. The
 * component `pages/agents/EditApplication.tsx` mounts is the same one this
 * page mounts, down to the "Set as default" item and its dialog; the
 * selector is even named `AgentPipelineVersionSelector` because the baseline
 * shared it across both domains. `pages/` -> `features/agents` via that
 * slice's `index.ts` is a downward, barrel-entered edge — the layer gate
 * permits it, and the alternative (a parallel pipelines copy) is how two
 * definitions of "which versions may be pinned" start drifting.
 *
 * **Version switching is a NAVIGATION.** `/pipelines/:tab/:agentId/:version`
 * already exists (ROUTE-069) and `useEditPipelineData` re-fetches the explicit
 * version whenever that param differs from the detail's default — so
 * navigating IS the switch, with no cache surgery and no second source of
 * truth for "which version am I on". `usePipelineVersionSync` then re-seeds
 * the flow editor from the newly-active version's `instructions`, which is
 * what makes the selector actually load a chosen version's GRAPH rather than
 * just relabel the header.
 *
 * **After a new version is created** two things happen that the agents twin
 * does not need. The application-detail response (the dropdown's source) is
 * stale by exactly one entry, so it is invalidated — same as agents. And the
 * live graph is carried onto the new version with a follow-up PUT. Main now
 * accepts geometry on creation, but this caller's POST still uses the stored
 * instructions, not the live canvas. The navigation happens LAST, so the editor
 * re-seeds from a version that already holds the graph instead of flashing
 * the pre-carry one.
 */
export function usePipelineVersionControls(args: PipelineVersionControlsArgs): PipelineVersionControlsState {
  const { projectId, applicationId, tab, versions, activeVersion } = args;
  const { control, versionFields, readGraphDraft, isReadOnly, isFetching, isGraphAdmissible } = args;

  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [versionError, setVersionError] = useState<string | undefined>(undefined);

  const versionOptions = useMemo(() => toVersionOptions(versions), [versions]);

  /*
   * The banner is scoped to the version it was raised on. `setVersionError`
   * used to be cleared ONLY inside the successful-carry branch, and nothing
   * else reset it — not a version switch, not an ordinary Save, not a later
   * Save-As-Version taken when `readGraphDraft()` returns `undefined` (that
   * branch skips the try/catch entirely). One transient 500 on the carry PUT
   * therefore pinned "the flow graph could not be copied" next to the version
   * dropdown for the rest of the page's life, still claiming it about a
   * version the user had long since navigated away from: this bar stays
   * mounted across every version navigation, because the `$version` route has
   * no component of its own.
   */
  const activeVersionKey = activeVersion?.id;
  useEffect(() => {
    setVersionError(undefined);
  }, [activeVersionKey]);

  const watchedStarters = useWatch({ control, name: 'version_details.conversation_starters' });

  const versionBody = useMemo(() => {
    if (activeVersion === undefined) return EMPTY_VERSION_BODY;
    return toNewPipelineVersionBody(activeVersion, (watchedStarters ?? []).filter(isString), versionFields);
  }, [activeVersion, watchedStarters, versionFields]);

  const goToVersion = useCallback(
    (versionId: number) => {
      if (applicationId === undefined) return;
      void navigate({
        to: '/pipelines/$tab/$agentId/$version',
        params: { tab: tab ?? 'latest', agentId: String(applicationId), version: String(versionId) },
      });
    },
    [navigate, applicationId, tab],
  );

  const invalidateDetail = useCallback(() => {
    if (projectId === undefined || applicationId === undefined) return;
    void queryClient.invalidateQueries({ queryKey: getGetApplicationQueryKey(projectId, applicationId) });
  }, [queryClient, projectId, applicationId]);

  const finishNewVersion = useCallback(
    async (created: ApplicationVersionDetail): Promise<void> => {
      const graph = readGraphDraft();
      const createdId = Number(created.id);
      /*
       * The same admission question `canSaveNewVersion` asks, re-asked at
       * carry time against the document actually about to be written. The
       * button is the primary gate; this is what stops a graph that went
       * inadmissible between the click and the POST's response from being
       * PUT onto the new version. Refusing the CARRY (rather than the whole
       * finish) is deliberate: the version already exists on the server, and
       * leaving it holding the previously stored — admissible — graph is the
       * only outcome that is not data loss.
       */
      if (graph !== undefined && !graph.admission.isAdmissible) {
        setVersionError(
          t(
            'pages.pipelines.editPipeline.versionGraphInadmissible',
            'The new version was created, but its flow graph was not copied onto it: the runtime would refuse that graph.',
          ),
        );
        invalidateDetail();
        disarmUnsavedChangesNavBlocker();
        goToVersion(createdId);
        return;
      }
      if (graph !== undefined && projectId !== undefined && applicationId !== undefined && !Number.isNaN(createdId)) {
        try {
          await carryPipelineGraphToVersion(queryClient, {
            projectId,
            applicationId,
            versionId: createdId,
            graph,
          });
          setVersionError(undefined);
        } catch {
          // The version EXISTS — it just holds the previously stored graph.
          // Reported rather than swallowed, and the navigation below still
          // happens so the user lands on the version they just made.
          setVersionError(
            t(
              'pages.pipelines.editPipeline.versionGraphCarryError',
              'The new version was created, but its flow graph could not be copied onto it.',
            ),
          );
        }
      }
      invalidateDetail();
      /*
       * The one navigation on this page that must NOT be second-guessed. The
       * page arms the app-wide unsaved-changes guard off its own dirty state
       * (`EditPipeline.tsx`'s `useUnsavedChangesNavBlocker`), and
       * `NavBlockerDialog`'s `shouldBlockFn` blocks any pathname change while
       * it is raised — including this one. Without the disarm the user got a
       * modal asking whether to discard the changes that had JUST been
       * persisted, and Cancel left the URL on the OLD version while the new
       * one silently held their work. `disarmUnsavedChangesNavBlocker`'s own
       * doc comment names this exact failure, and `EditPipeline`'s discard
       * path already calls it; the two version-bar navigations were the ones
       * that did not.
       *
       * It takes effect in this same handler because `shouldBlockFn` reads
       * the live store snapshot rather than a closed-over render value.
       */
      disarmUnsavedChangesNavBlocker();
      goToVersion(createdId);
    },
    [readGraphDraft, projectId, applicationId, queryClient, invalidateDetail, goToVersion],
  );

  const handleNewVersionSaved = useCallback(
    (created: ApplicationVersionDetail) => {
      void finishNewVersion(created);
    },
    [finishNewVersion],
  );

  /*
   * After the open version is deleted the URL still points at it and
   * `useEditPipelineData` would 404 on the next fetch. Navigate to the
   * pipeline's version-less route (its default version) and invalidate the
   * detail, whose `versions[]` is stale by exactly the deleted entry — the
   * mirror image of `finishNewVersion` above.
   */
  const handleVersionDeleted = useCallback(() => {
    invalidateDetail();
    if (applicationId === undefined) return;
    /*
     * Same disarm as `finishNewVersion`, and the failure it prevents is
     * worse here: this navigation is an ESCAPE, not a convenience. Cancelling
     * the spurious "unsaved changes" dialog stranded the user on the URL of a
     * version that no longer exists — the refreshed detail's `versions[]` no
     * longer lists it, so `useIsVersionNotFound` flips and the page falls to
     * `EditPipelineNotFound kind="version"` with no way back except editing
     * the URL. The comment above states the whole point of this navigation is
     * that "`useEditPipelineData` would 404 on the next fetch", which is
     * exactly what a block leaves in place.
     */
    disarmUnsavedChangesNavBlocker();
    void navigate({
      to: '/pipelines/$tab/$agentId',
      params: { tab: tab ?? 'latest', agentId: String(applicationId) },
    });
  }, [invalidateDetail, navigate, applicationId, tab]);

  const versionDelete = useMemo(() => {
    if (activeVersion === undefined) return undefined;
    return {
      onVersionDeleted: handleVersionDeleted,
      /*
       * The banner is the SECOND place the refusal appears. `DeleteVersionDialog`
       * shows it inside the confirm dialog too (#147); before that slot
       * existed this callback was the only channel, and omitting it left a
       * refused delete — the server answers "Unpublish first. Cannot delete a
       * published version." for a published or embedded version — with the
       * dialog just sitting there, spinner off, and every re-Confirm failing
       * silently. The banner and its `reportVersionError` setter were already
       * wired for the other two version writes; this was the third.
       *
       * The version to delete is no longer named here: #147 moved the
       * trigger into the version menu, which acts on whichever version that
       * menu marks as selected.
       */
      onVersionDeleteError: setVersionError,
    };
  }, [activeVersion, handleVersionDeleted]);

  const handleSelectVersion = useCallback(
    (version: EditPipelineVersionOption) => goToVersion(version.id),
    [goToVersion],
  );

  return {
    showVersionControls: !isFetching && applicationId !== undefined,
    applicationIdText: applicationId === undefined ? '' : String(applicationId),
    versionOptions,
    activeVersionId: activeVersion === undefined ? undefined : Number(activeVersion.id),
    versionBody,
    canSaveNewVersion: !isReadOnly && activeVersion !== undefined,
    isSaveNewVersionBlocked: !isGraphAdmissible,
    handleSelectVersion,
    handleNewVersionSaved,
    versionDelete,
    versionError,
    reportVersionError: setVersionError,
  };
}
