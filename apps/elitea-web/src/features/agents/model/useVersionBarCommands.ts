import { useCallback, useState } from 'react';

import type { AgentPipelineVersionOption } from '../lib/types';

import { useSetDefaultVersion } from './useSetDefaultVersion';

/**
 * The state behind the two commands the agent version menu offers: "Set as
 * default" (#147, first half) and "Delete version" (#147, second half).
 *
 * SPLIT OUT OF `AgentVersionControls` FOR THE §3.5 COMPLEXITY BUDGET (≤12).
 * With both flows inline that component measured 17. Nothing here is
 * presentation: it is which version each command is pending on, which version
 * the menu marks as the default, and which versions the menu must stop
 * offering because this component has already seen them deleted.
 *
 * The hook returns finished prop objects rather than raw state, so the
 * component spreads them and carries no `?.`/`??`/ternary of its own for
 * either dialog. That is what keeps the budget met; it also means the "is
 * this dialog open" rule lives in exactly one place.
 */
export interface VersionBarCommandsInput {
  readonly applicationId: string;
  readonly projectId: string | undefined;
  readonly versions: readonly AgentPipelineVersionOption[];
  /** `false` for a read-only viewer: both commands go, the version list stays. */
  readonly canWrite: boolean;
  readonly versionDelete:
    | {
        readonly onVersionDeleted: () => void;
        readonly onVersionDeleteError?: ((message: string) => void) | undefined;
      }
    | undefined;
}

/** Exactly `SetDefaultVersionDialog`'s props. */
interface SetDefaultDialogProps {
  readonly open: boolean;
  readonly versionName: string;
  readonly confirming: boolean;
  readonly errorMessage: string | undefined;
  readonly onClose: () => void;
  readonly onConfirm: () => void;
}

/** Exactly `DeleteVersionDialog`'s props. */
interface DeleteDialogProps {
  readonly open: boolean;
  readonly projectId: string | undefined;
  readonly applicationId: number;
  readonly versionId: number | undefined;
  readonly versionName: string;
  readonly onClose: () => void;
  readonly onDeleted: () => void;
  readonly onError: ((message: string) => void) | undefined;
}

export interface VersionBarCommands {
  /** The version list the menu renders — see `withoutDeleted`. */
  readonly visibleVersions: readonly AgentPipelineVersionOption[];
  readonly defaultVersionId: number | undefined;
  /** `undefined` when the command is not on offer, which is how the menu decides whether to render the item at all. */
  readonly onSetDefaultVersion: ((version: AgentPipelineVersionOption) => void) | undefined;
  readonly onDeleteVersion: ((version: AgentPipelineVersionOption) => void) | undefined;
  readonly setDefaultDialog: SetDefaultDialogProps;
  readonly deleteDialog: DeleteDialogProps;
}

/**
 * Which version the menu marks as the default.
 *
 * `justSet` — the id of a PATCH this hook has already seen succeed — wins over
 * the flag on the options, because in the window before the detail is
 * re-fetched those options are stale by exactly that write. With no such
 * write, the server's own answer decides.
 *
 * `find` rather than `filter`: exactly one version may carry the flag (the Go
 * handler derives every row's from the single `applications.meta.
 * default_version_id`), so a second one would be a database inconsistency, not
 * a case to render. `=== true` so an option list that omits the field reads as
 * "this list cannot say" rather than as a truthiness accident.
 */
function resolveDefaultVersionId(
  versions: readonly AgentPipelineVersionOption[],
  justSet: number | undefined,
): number | undefined {
  if (justSet !== undefined) return justSet;
  return versions.find((version) => version.is_default === true)?.id;
}

/**
 * The version list the menu renders, with the versions this hook has already
 * seen deleted taken out — #147's optimistic update.
 *
 * The delete succeeds against the server, and the `versions` prop is rebuilt
 * from an application-detail query the page then invalidates. Between the two
 * the prop still holds the row that is gone, so without this the menu keeps
 * offering a version whose next fetch answers 404.
 */
function withoutDeleted(
  versions: readonly AgentPipelineVersionOption[],
  deletedIds: readonly number[],
): readonly AgentPipelineVersionOption[] {
  if (deletedIds.length === 0) return versions;
  return versions.filter((version) => !deletedIds.includes(version.id));
}

/**
 * One confirmed set-default attempt.
 *
 * Module-level, not inline in the `useCallback` below, for the §3.5
 * cyclomatic-complexity budget (≤12) — the hook measured 13 with these two
 * guards inside it.
 */
async function runSetDefault(
  version: AgentPipelineVersionOption | undefined,
  doSetDefaultVersion: (versionId: number) => Promise<boolean>,
  onDone: (versionId: number) => void,
): Promise<void> {
  if (version === undefined) return;
  const ok = await doSetDefaultVersion(version.id);
  // Nothing happens on failure: the default is unchanged, the dialog stays
  // open (closing it would read as "done"), and `errorMessage` carries the
  // server's own refusal.
  if (!ok) return;
  onDone(version.id);
}

/**
 * `DeleteVersionDialog`'s props, resolved from the pending version.
 *
 * Module-level for the §3.5 cyclomatic-complexity budget, like `runSetDefault`
 * above: the four `?.`/`??` reads it holds were what put the hook over.
 */
function toDeleteDialogProps(params: {
  readonly enabled: boolean;
  readonly pending: AgentPipelineVersionOption | undefined;
  readonly projectId: string | undefined;
  readonly applicationId: string;
  readonly onClose: () => void;
  readonly onDeleted: () => void;
  readonly onError: ((message: string) => void) | undefined;
}): DeleteDialogProps {
  return {
    open: params.enabled && params.pending !== undefined,
    projectId: params.projectId,
    applicationId: Number(params.applicationId),
    versionId: params.pending?.id,
    versionName: params.pending?.name ?? '',
    onClose: params.onClose,
    onDeleted: params.onDeleted,
    onError: params.onError,
  };
}

export function useVersionBarCommands(input: VersionBarCommandsInput): VersionBarCommands {
  const { applicationId, projectId, versions, canWrite, versionDelete } = input;

  const [pendingDefault, setPendingDefault] = useState<AgentPipelineVersionOption | undefined>(undefined);
  const [justSetDefaultId, setJustSetDefaultId] = useState<number | undefined>(undefined);
  const [pendingDelete, setPendingDelete] = useState<AgentPipelineVersionOption | undefined>(undefined);
  const [deletedVersionIds, setDeletedVersionIds] = useState<readonly number[]>([]);

  // The hook's ids are non-optional; neither command is offered until
  // `projectId` resolves, so the placeholder below is never the one a request
  // is made with — the same guard `DeleteVersionDialog` states for its ids.
  const { doSetDefaultVersion, isSettingDefaultVersion, errorMessage, resetError } = useSetDefaultVersion({
    projectId: projectId ?? '',
    applicationId: Number(applicationId),
  });

  const requestSetDefault = useCallback(
    (version: AgentPipelineVersionOption) => {
      resetError();
      setPendingDefault(version);
    },
    [resetError],
  );

  const confirmSetDefault = useCallback((): void => {
    void runSetDefault(pendingDefault, doSetDefaultVersion, (versionId) => {
      setJustSetDefaultId(versionId);
      setPendingDefault(undefined);
    });
  }, [doSetDefaultVersion, pendingDefault]);

  /*
   * The optimistic step, then the caller's own orchestration. The order
   * matters: `onVersionDeleted` navigates away from the deleted version, and
   * doing that first would unmount the bar before the list it renders could
   * drop the row.
   */
  const handleDeleted = useCallback(() => {
    if (pendingDelete !== undefined) {
      setDeletedVersionIds((previous) => [...previous, pendingDelete.id]);
    }
    setPendingDelete(undefined);
    versionDelete?.onVersionDeleted();
  }, [pendingDelete, versionDelete]);

  const closeSetDefault = useCallback(() => setPendingDefault(undefined), []);
  const closeDelete = useCallback(() => setPendingDelete(undefined), []);

  const canSetDefault = canWrite && projectId !== undefined;
  const canDelete = canSetDefault && versionDelete !== undefined;

  return {
    visibleVersions: withoutDeleted(versions, deletedVersionIds),
    defaultVersionId: resolveDefaultVersionId(versions, justSetDefaultId),
    onSetDefaultVersion: canSetDefault ? requestSetDefault : undefined,
    onDeleteVersion: canDelete ? setPendingDelete : undefined,
    setDefaultDialog: {
      open: pendingDefault !== undefined,
      versionName: pendingDefault?.name ?? '',
      confirming: isSettingDefaultVersion,
      errorMessage,
      onClose: closeSetDefault,
      onConfirm: confirmSetDefault,
    },
    deleteDialog: toDeleteDialogProps({
      // A dialog that is never opened sends nothing, so it is mounted
      // unconditionally rather than guarded by `canDelete`. `open` is the
      // only gate that matters, and keeping it the only gate means the
      // component carries no second copy of the same rule.
      enabled: canDelete,
      pending: pendingDelete,
      projectId,
      applicationId,
      onClose: closeDelete,
      onDeleted: handleDeleted,
      onError: versionDelete?.onVersionDeleteError,
    }),
  };
}
