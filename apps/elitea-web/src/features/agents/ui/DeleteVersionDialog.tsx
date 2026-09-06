import type { ReactNode } from 'react';
import { useCallback, useEffect, useState } from 'react';

import { t } from '@/shared/i18n';
import { DeleteEntityModal } from '@/shared/ui/DeleteEntityModal';

import { useDeleteVersion } from '../model/useDeleteVersion';

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
 * **The baseline's in-use branch is deliberately NOT wired, and the reason
 * is a backend defect, not a porting shortcut.** The baseline runs
 * `doCheckVersionInUse` first and, when the version is referenced by other
 * agents/pipelines, opens `VersionReplacementModal` to repoint those
 * references before deleting. The Go endpoint behind that check
 * (`GET /check_version_in_use/...` -> `eliteacore.ApplicationRelation`,
 * handler.go:1590-1627) answers a DIFFERENT question: it selects
 * `entity_skill_mapping`/`entity_tool_mapping` rows WHERE
 * `entity_version_id = <this version>` — the skills and tools THIS version
 * uses, not the parents that reference it. So (a) `isInUse` is true for
 * essentially every non-trivial version, which would put a
 * "choose a replacement" modal in front of every ordinary delete, and
 * (b) the rows it returns are `{type, id}` only (confirmed in the generated
 * contract, `applicationRelationList.zod.ts`) — they carry no
 * `application_name`/`version_name`, which is exactly what that modal lists.
 * Wiring it would produce a confident-looking dialog full of wrong data.
 * Until the endpoint answers the inverse question, this deletes directly and
 * surfaces whatever the delete endpoint itself refuses.
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

  useEffect(() => {
    if (!open) setRefusal(undefined);
  }, [open]);

  // The hook's ids are non-optional; the menu item that opens this dialog is
  // only offered once they resolve, so the placeholders below are never the
  // ones a request is made with.
  const { doDeleteVersion, isDeletingVersion } = useDeleteVersion({
    projectId: projectId ?? '',
    applicationId: applicationId ?? 0,
    versionId: versionId ?? 0,
  });

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

  return (
    <DeleteEntityModal
      open={open}
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
