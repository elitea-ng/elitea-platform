import type { ReactNode } from 'react';
import { useCallback, useEffect, useMemo, useState } from 'react';

import { t } from '@/shared/i18n';
import { DeleteEntityModal } from '@/shared/ui/DeleteEntityModal';

import { useDeleteVersion } from '../model/useDeleteVersion';
import type { UseDeleteVersionResult } from '../model/useDeleteVersion';
import { VersionReplacementModal } from './VersionReplacementModal';

/**
 * The confirm step of #147's delete action, and the only place this app
 * sends `DELETE /elitea_core/version/prompt_lib/{p}/{app}/{ver}`.
 *
 * It replaces `DeleteVersionButton`, an icon button that sat BESIDE the
 * version selector. #147 asks for the affordance the baseline has: an item
 * inside the version menu, next to "Set as a default"
 * (`apps/elitea-ui/src/[fsd]/entities/application-tab-bar/ui/
 * ApplicationControls.jsx:145-156`). A menu item cannot own its own dialog
 * and stay inside the `Menu`, so the trigger moved to
 * `AgentPipelineVersionSelector` and the dialog moved here. Keeping both the
 * button and the item would give one action two controls.
 *
 * **TYPE THE NAME TO CONFIRM.** The delete is not reversible and the version
 * menu offers it one click from the version list, so `shouldRequestInputName`
 * makes the user write the version name first. The baseline guards the same
 * action with a two-step in-use check; this app cannot run that check (see
 * below), so the safeguard is the typed name.
 *
 * **The in-use branch (#894).** Opening this dialog runs
 * `doCheckVersionInUse` FIRST, and the answer decides which of the two
 * dialogs below is shown: `VersionReplacementModal` ("Version in use", with
 * the referencing agents listed by name and a replacement picker) when
 * another version references this one as a sub-agent, and the plain
 * type-the-name confirm otherwise.
 *
 * That branch could not be wired for the whole life of this file, and the
 * reason was a backend defect rather than a porting shortcut: the endpoint
 * behind the check (`GET /check_version_in_use/...` ->
 * `eliteacore.ApplicationRelation`) answered the OPPOSITE question — the
 * skills and tools THIS version uses, keyed `entity_version_id = <this
 * version>`, not the parents that reference it — so `isInUse` was true for
 * essentially every non-trivial version, and the `{type, id}` rows it
 * returned carried none of the names that modal lists. Both halves are fixed
 * in the handler now (`in_use`, `referencing_parents`,
 * `replacement_versions`), and `DELETE /version/...` honours
 * `replacement_version_id` so "Replace & Delete" is one call.
 *
 * Neither dialog is rendered until the check has answered. A dialog that
 * appeared first and then swapped itself for a different one would be worse
 * than a short wait: the user would be reading a confirm box that is about
 * to become something else. A check that FAILS falls back to the plain
 * confirm — the state this file was in before, and the safe one.
 *
 * **The refusal reaches the user (#147, work item 1).** The server answers
 * 400 `{"error": "Unpublish first. Cannot delete a published version."}` for
 * a published or embedded version
 * (`services/elitea-main/internal/api/v2/applications/handler.go:1223-1235`,
 * proved by `delete_version_postgres_integration_test.go`). That message used
 * to reach `DeleteVersionButton`'s `onError?` callback, which every caller
 * left unset, and `DeleteEntityModal` had nowhere to show it — so a refused
 * delete left the dialog open, silent. The dialog stays open on a refusal on
 * purpose (closing it would read as "deleted") and now names the reason.
 * `onError` still fires, for a page that wants to report it somewhere else
 * as well.
 *
 * Caller-owned orchestration, matching `useDeleteVersion`'s own contract: no
 * navigation and no cache invalidation here — success is reported through
 * `onDeleted` and the page decides where to go.
 */
/**
 * What the in-use check answered, as the hook types it. Read off the hook's
 * own return type rather than re-declared: `CheckVersionInUseResult` is
 * deliberately not exported (knip's unused-export discipline — see its own
 * comment), and a second hand-written copy of the shape here is exactly the
 * kind of drift that makes two files disagree about one response.
 */
type VersionUsage = Awaited<ReturnType<UseDeleteVersionResult['doCheckVersionInUse']>>;

export interface DeleteVersionDialogProps {
  readonly open: boolean;
  readonly projectId: string | undefined;
  readonly applicationId: number | undefined;
  readonly versionId: number | undefined;
  readonly versionName: string;
  readonly onClose: () => void;
  readonly onDeleted: () => void;
  readonly onError?: ((message: string) => void) | undefined;
}

export function DeleteVersionDialog({
  open,
  projectId,
  applicationId,
  versionId,
  versionName,
  onClose,
  onDeleted,
  onError,
}: DeleteVersionDialogProps): ReactNode {
  /* The dialog's own copy of the refusal. `useDeleteVersion` keeps its
     `error` until the next attempt, so a message from a PREVIOUS version
     would otherwise greet the user the moment this dialog reopens. */
  const [refusal, setRefusal] = useState<string | undefined>(undefined);
  /* #894 — the in-use answer for THIS opening. `checked` is separate from
     `usage` because "the check failed" and "nothing references it" are
     different states that render the same dialog for different reasons. */
  const [usage, setUsage] = useState<VersionUsage | undefined>(undefined);
  const [checked, setChecked] = useState(false);

  useEffect(() => {
    if (!open) setRefusal(undefined);
  }, [open]);

  // The hook's ids are non-optional; the menu item that opens this dialog is
  // only offered once they resolve, so the placeholders below are never the
  // ones a request is made with.
  const { doCheckVersionInUse, doDeleteVersion, isDeletingVersion } = useDeleteVersion({
    projectId: projectId ?? '',
    applicationId: applicationId ?? 0,
    versionId: versionId ?? 0,
  });

  // The check runs on every OPENING, not once per mount: this component stays
  // mounted between deletes (the version bar composes it), and a cached answer
  // would describe whichever version was pending last time.
  useEffect(() => {
    if (!open) {
      setUsage(undefined);
      setChecked(false);
      return;
    }
    let cancelled = false;
    void (async () => {
      const result = await doCheckVersionInUse();
      if (cancelled) return;
      setUsage(result);
      setChecked(true);
    })();
    return () => {
      cancelled = true;
    };
  }, [open, doCheckVersionInUse]);

  const referencingParents = useMemo(
    () =>
      (usage?.referencingParents ?? []).map((parent) => ({
        application_id: parent.application_id ?? 0,
        version_id: parent.version_id ?? 0,
        application_name: parent.application_name ?? '',
        version_name: parent.version_name ?? '',
      })),
    [usage],
  );
  const replacementVersions = useMemo(
    () =>
      (usage?.replacementVersions ?? []).map((version) => ({
        id: version.id ?? 0,
        name: version.name ?? '',
        created_at: version.created_at ?? undefined,
      })),
    [usage],
  );

  /*
   * The message comes off the OUTCOME, not off the hook's `errorMessage`
   * state. `handleConfirm` is a closure created at the previous render, so
   * the state field it can see is the one from BEFORE this attempt failed —
   * `DeleteVersionButton` read it that way and therefore reported the
   * generic fallback for every first refusal. See `DeleteVersionOutcome`.
   */
  const handleConfirm = useCallback(async (): Promise<void> => {
    setRefusal(undefined);
    const outcome = await doDeleteVersion();
    if (!outcome.ok) {
      const message = outcome.errorMessage ?? t('features.agents.deleteVersion.error', 'Failed to delete this version.');
      setRefusal(message);
      onError?.(message);
      return;
    }
    onDeleted();
  }, [doDeleteVersion, onDeleted, onError]);

  /* "Replace & Delete" — one call, the delete route carrying the replacement
     id (see `useDeleteVersion`). A refusal keeps the modal open, the way the
     plain dialog's does; the message reaches the caller through `onError`,
     which is where this dialog's host renders it. */
  const handleReplace = useCallback(
    (replacementId: number | string): void => {
      void (async () => {
        setRefusal(undefined);
        const outcome = await doDeleteVersion(Number(replacementId));
        if (!outcome.ok) {
          const message = outcome.errorMessage ?? t('features.agents.deleteVersion.error', 'Failed to delete this version.');
          setRefusal(message);
          onError?.(message);
          return;
        }
        onDeleted();
      })();
    },
    [doDeleteVersion, onDeleted, onError],
  );

  const isInUse = checked && usage?.isInUse === true;

  if (isInUse) {
    return (
      <VersionReplacementModal
        open
        onClose={onClose}
        versionName={versionName}
        referencingParents={referencingParents}
        replacementVersions={replacementVersions}
        onReplace={handleReplace}
        isReplacing={isDeletingVersion}
      />
    );
  }

  return (
    <DeleteEntityModal
      open={open && checked}
      onClose={onClose}
      onConfirm={() => {
        void handleConfirm();
      }}
      confirming={isDeletingVersion}
      name={versionName}
      shouldRequestInputName
      data-testid="agent-version-delete-dialog"
      copy={{ title: t('features.agents.deleteVersion.title', 'Delete version') }}
      {...(refusal === undefined ? {} : { errorMessage: refusal })}
    />
  );
}
