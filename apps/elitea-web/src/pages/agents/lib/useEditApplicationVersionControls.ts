import { useCallback, useMemo } from 'react';

import { useNavigate } from '@tanstack/react-router';
import { useQueryClient } from '@tanstack/react-query';
import { useWatch, type Control } from 'react-hook-form';

import type { ApplicationCreationInput } from '@/entities/application-form';
import { disarmUnsavedChangesNavBlocker } from '@/widgets/app-shell';
import { getGetApplicationQueryKey } from '@/shared/api/generated/applications/applications';
import type {
  ApplicationVersionDetail,
  ApplicationVersionSummary,
  VersionWriteRequest,
} from '@/shared/api/generated/model';

import {
  toVersionOptions,
  toVersionWriteBody,
  type EditApplicationVersionOption,
} from './editApplicationMappers';
import type { EditApplicationVersionFields } from './useEditApplicationVersionFields';

export interface EditApplicationVersionControlsArgs {
  readonly projectId: string | undefined;
  readonly applicationId: number | undefined;
  readonly tab: string | undefined;
  readonly versions: readonly ApplicationVersionSummary[];
  readonly activeVersion: ApplicationVersionDetail | undefined;
  /**
   * The page's RHF control. Conversation starters are read LIVE off it
   * (`useWatch`), not off `activeVersion`, because the baseline clones the
   * CURRENTLY EDITED `version_details` onto a new version
   * (`SaveNewVersionButton.jsx`) — starters typed but not yet saved must
   * travel with it.
   */
  readonly control: Control<ApplicationCreationInput>;
  /**
   * The live version-level edits, for the same reason `control` is taken
   * above: they are cloned onto the new version. #307 — until those fields
   * were routed anywhere they could not differ from `activeVersion`, so
   * omitting them was invisible; now a "Save As Version" taken after
   * editing the instructions would otherwise clone the STORED ones.
   */
  readonly versionFields: EditApplicationVersionFields;
  /** Public-project viewer: the selector stays, the write affordance goes (`ApplicationTabBar.jsx:65`). */
  readonly isReadOnly: boolean;
  /** While the detail is in flight there is neither a version list nor an active version to show. */
  readonly isFetching: boolean;
}

export interface EditApplicationVersionControlsState {
  /** `false` until there is a real agent id and the detail has settled — the page's single render gate, kept here so that page's own cyclomatic budget (§3.5, 12) is not spent on it. */
  readonly showVersionControls: boolean;
  /** The agent id in the string form `SaveNewVersionButton` takes. `''` only while `showVersionControls` is `false`, i.e. never rendered. */
  readonly applicationIdText: string;
  readonly versionOptions: readonly EditApplicationVersionOption[];
  readonly activeVersionId: number | undefined;
  readonly versionBody: Omit<VersionWriteRequest, 'name'>;
  readonly canSaveNewVersion: boolean;
  readonly handleSelectVersion: (version: EditApplicationVersionOption) => void;
  readonly handleNewVersionSaved: (created: ApplicationVersionDetail) => void;
  /**
   * #307/#147 — version delete: what to do once a version is gone, and where
   * to report a refusal.
   *
   * The version itself is NOT named here. #147 moved the trigger into the
   * version menu, which acts on whichever version that menu marks as
   * selected; naming the page's active version here could only ever delete
   * the one the user was already on.
   *
   * `onVersionDeleteError` was the missing half of #147: the server refuses a
   * published or embedded version with 400 "Unpublish first. Cannot delete a
   * published version.", this type did not declare the callback that carries
   * it, and `DeleteEntityModal` had no error slot, so the refusal reached
   * nobody.
   *
   * IT IS STILL NOT SUPPLIED HERE, and that is the fix rather than the gap.
   * The message is now shown INSIDE the confirm dialog, which is where the
   * user is standing when the refusal happens. This page has no banner and
   * no toast to put a second copy in, so declaring the callback and passing
   * a handler with nowhere to render would be one more dead wire. The
   * pipelines twin (`pages/pipelines/lib/usePipelineVersionControls.ts`)
   * DOES supply it, because that page already owns a version-error banner
   * for its two other version writes.
   */
  readonly versionDelete: { readonly onVersionDeleted: () => void } | undefined;
}

/** `conversation_starters` is typed as a loose array on the form input; the write body takes `string[]`. */
function isString(entry: unknown): entry is string {
  return typeof entry === 'string';
}

/** Stable empty body for the (transient) window before the active version has resolved — a fresh object literal each render would make `versionBody` a new prop reference on every render of the page. */
const EMPTY_VERSION_BODY: Omit<VersionWriteRequest, 'name'> = {};

/**
 * The page-side half of #134's version bar: everything
 * `features/agents`' deliberately-dumb `AgentVersionControls` refuses to own
 * — which route a version switch navigates to, what body a "Save As Version"
 * clones, and what happens to the cache once one is created.
 *
 * Split out of `EditApplication.tsx` for the same §3.5 file-length and
 * oxlint-complexity reasons as its `useEditApplicationData`/
 * `useEditApplicationForm` siblings in this directory.
 *
 * **Version switching is a NAVIGATION, not a local state change.** The
 * baseline's `ApplicationVersionSelect` mutates an RTK-Query cache entry in
 * place (`eliteaApi.util.updateQueryData`) and separately keeps the URL's
 * `:version` segment in sync; this app already routes `:version`
 * (ROUTE-067, `routes/_shell/agents/$tab.$agentId.$version.tsx`) and
 * `useEditApplicationData` already re-fetches the explicit version whenever
 * that param differs from the detail's default version — so navigating IS
 * the switch, with no cache surgery and no second source of truth for
 * "which version am I on".
 *
 * **After a new version is created** the application-detail response (which
 * carries `versions[]`, the dropdown's source) is stale by exactly one
 * entry, and `useSaveNewVersion` deliberately invalidates nothing (see its
 * own doc comment). Invalidate it here, then navigate onto the new version —
 * the same order the baseline's `onSuccess` handler uses (refetch details,
 * then move the URL onto the created version).
 */
export function useEditApplicationVersionControls(
  args: EditApplicationVersionControlsArgs,
): EditApplicationVersionControlsState {
  const { projectId, applicationId, tab, versions, activeVersion, control, versionFields, isReadOnly, isFetching } = args;

  const navigate = useNavigate();
  const queryClient = useQueryClient();

  const versionOptions = useMemo(() => toVersionOptions(versions), [versions]);

  const watchedStarters = useWatch({ control, name: 'version_details.conversation_starters' });

  const versionBody = useMemo(() => {
    if (activeVersion === undefined) return EMPTY_VERSION_BODY;
    return toVersionWriteBody(activeVersion, (watchedStarters ?? []).filter(isString), versionFields);
  }, [activeVersion, watchedStarters, versionFields]);

  const goToVersion = useCallback(
    (versionId: number) => {
      if (applicationId === undefined) return;
      void navigate({
        to: '/agents/$tab/$agentId/$version',
        params: { tab: tab ?? 'latest', agentId: String(applicationId), version: String(versionId) },
      });
    },
    [navigate, applicationId, tab],
  );

  const handleSelectVersion = useCallback(
    (version: EditApplicationVersionOption) => goToVersion(version.id),
    [goToVersion],
  );

  const handleNewVersionSaved = useCallback(
    (created: ApplicationVersionDetail) => {
      if (projectId !== undefined && applicationId !== undefined) {
        void queryClient.invalidateQueries({ queryKey: getGetApplicationQueryKey(projectId, applicationId) });
      }
      /*
       * #133 — `EditApplication.tsx` arms the app-wide unsaved-changes guard
       * off its own `isDirty`, and `NavBlockerDialog` blocks ANY pathname
       * change while it is raised. Without this disarm the post-save
       * navigation onto the version just created was intercepted by a modal
       * asking whether to discard the changes it had just persisted, and
       * Cancel left the URL on the old version while the new one silently
       * held the work. `disarmUnsavedChangesNavBlocker`'s own doc comment
       * names this failure; `EditApplicationActions` already calls it on the
       * discard path. The pipelines twin
       * (`pages/pipelines/lib/usePipelineVersionControls.ts`) carries the
       * same pair of calls for the same reason.
       */
      disarmUnsavedChangesNavBlocker();
      goToVersion(Number(created.id));
    },
    [queryClient, projectId, applicationId, goToVersion],
  );

  /*
   * #307 — after the open version is deleted the URL still points at it, and
   * `useEditApplicationData` would 404 on the next fetch. Navigate to the
   * agent's version-less route (its default version) and invalidate the
   * detail, whose `versions[]` is now stale by exactly the deleted entry —
   * the mirror image of `handleNewVersionSaved` above.
   */
  const handleVersionDeleted = useCallback(() => {
    if (projectId !== undefined && applicationId !== undefined) {
      void queryClient.invalidateQueries({ queryKey: getGetApplicationQueryKey(projectId, applicationId) });
    }
    if (applicationId === undefined) return;
    /*
     * Same disarm, worse failure: this navigation is an ESCAPE. Blocking it
     * strands the user on the URL of a version that no longer exists, which
     * is precisely the 404 the comment above says it exists to avoid.
     */
    disarmUnsavedChangesNavBlocker();
    void navigate({
      to: '/agents/$tab/$agentId',
      params: { tab: tab ?? 'latest', agentId: String(applicationId) },
    });
  }, [queryClient, navigate, projectId, applicationId, tab]);

  const versionDelete = useMemo(() => {
    if (activeVersion === undefined) return undefined;
    return { onVersionDeleted: handleVersionDeleted };
  }, [activeVersion, handleVersionDeleted]);

  return {
    versionDelete,
    showVersionControls: !isFetching && applicationId !== undefined,
    applicationIdText: applicationId === undefined ? '' : String(applicationId),
    versionOptions,
    activeVersionId: activeVersion === undefined ? undefined : Number(activeVersion.id),
    versionBody,
    canSaveNewVersion: !isReadOnly && activeVersion !== undefined,
    handleSelectVersion,
    handleNewVersionSaved,
  };
}
