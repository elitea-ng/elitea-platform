import type { ReactNode } from 'react';
import { useCallback, useMemo } from 'react';

import type { ToolkitWriteResult, UseToolkitCreateMutation, UseToolkitEditMutation } from '../api/toolkits';
import { useToolkitSaveValidation } from '../model/useToolkitSaveValidation';

import { useToolkitEditorNotice } from './ToolkitEditorNotice';

import type { ToolkitFormValues } from './SaveToolkitButton';
import type { ToolkitFormEditDetail } from './form/ToolkitForm/ToolkitForm';
import {
  EMPTY_PARTICIPANT,
  ToolkitEditorBody,
  ToolkitEditorSaveButton,
  emptyContentSx,
  resolveToolkitFormDisabled,
  resolveToolkitName,
  useToolkitEditorState,
} from './ToolkitEditorParts';
import type { ToolkitEditorParticipant, ToolkitEditorProps } from './ToolkitEditorParts';

export type { ToolkitEditorDeps, ToolkitEditorParticipant, ToolkitEditorProps, ToolkitEditorShellProps } from './ToolkitEditorParts';

/**
 * Ported from `apps/elitea-ui/src/pages/NewChat/ToolkitEditor.jsx` (335
 * lines) — unit A4g.
 *
 * **MUST be exported through `src/features/toolkits/index.ts`** — Wave-2
 * unit C6 needs it cross-feature (this batch's own mission brief).
 *
 * **Real, load-bearing redesign, not a porting shortcut — read before
 * wiring this up.** Follows the EXACT `deps`-injection shape
 * `features/agents/ui/AgentEditor.tsx`/`features/pipelines/ui/
 * PipelineEditor.tsx` already established for this batch:
 *
 *  - `deps.renderShell` — the chat-owned editor chrome (baseline:
 *    `pages/NewChat/components/BaseEditor.jsx`). `features/toolkits`
 *    cannot import `features/chat` (`no-sideways-features`, no carve-out;
 *    the mission brief's own framing: "Wave-2 unit C6 (a much later chat
 *    unit)" confirms this is real future work, not a landed sibling).
 *  - `deps.createToolkit`/`deps.saveToolkit` — no generated `POST
 *    /elitea_core/tools/prompt_lib/{projectId}` or `PUT /elitea_core/tool/
 *    prompt_lib/{projectId}/{toolId}` endpoint exists (see `../api/
 *    toolkits.ts`'s module doc comment for the full, exhaustively-verified
 *    inventory). Injected exactly like `useValidateToolkit`'s
 *    `useValidateToolkitQuery` — this component owns 100% real orchestration
 *    (which button renders, dirty/loading state, credential-warning gating)
 *    around a network call a future caller supplies once a real endpoint
 *    exists.
 *  - `deps.checkBeforeSave` — baseline: `entities/credential-warning`'s
 *    `useCredentialWarning`/`CredentialWarningModal`. `features/toolkits`
 *    cannot import `features/credentials` (`no-sideways-features`), AND
 *    that slice's own `index.ts` does not export `CredentialWarningModal`/
 *    `useCredentialWarningModal` even if it could (§3.5 budget — see that
 *    file's own doc comment: "NOT exported here despite being fully built
 *    and tested"). Rather than half-plumb a modal-rendering slot through
 *    this component for a dependency it cannot reach either way, the ENTIRE
 *    check (including any modal UI) is the caller's opaque
 *    `(performSave) => boolean` callback — identical contract
 *    `SaveToolkitButton.tsx`'s own `onBeforeSave` prop already documents.
 *    Omitted entirely, saving proceeds immediately (matches the baseline's
 *    own `if (onBeforeSave) {...} else { await performSave() }` no-op
 *    default).
 *  - `deps.onEditorClosed` — baseline: none directly (the toolkit editor's
 *    close handler is caller-supplied `onCloseToolkitEditor` already);
 *    matches `AgentEditorDeps`'s identical optional hook for symmetry with
 *    the two sibling editors.
 *
 * **Dropped, disclosed gaps (same "no unreachable dependency invented"
 * discipline as `AgentEditor.tsx`/`PipelineEditor.tsx`):**
 *  - `usePublicProjectAccessCheck` (`features/project/lib/hooks` in the
 *    baseline) has no port anywhere in this worktree — `features/project`
 *    does not exist as a landed slice, and `entities/project`'s public API
 *    (`isPublicProject`/`isSuspendedProject`/`sortProjectsByName`) has no
 *    "does the current user have write access to the public project"
 *    check. Rather than dropping the baseline's `isPublic &&
 *    !hasPublicProjectAccess` formula outright, `deps.hasPublicProjectAccess`
 *    (optional, see `ToolkitEditorDeps`'s own doc comment) is the injection
 *    point a future caller with a real permission source can supply — same
 *    "inject the actual capability" convention `deps.checkBeforeSave`
 *    already uses. Omitted (every caller today), `disabled` reduces to
 *    `isPublic` alone (edit mode always locks a public-project toolkit) —
 *    a conservative default (never LESS locked than the baseline), not an
 *    invented permission model.
 *  - GA event tracking (`useTrackEvent`, baseline lines 5,30,71-79) —
 *    dropped outright, same documented gap `AgentEditor.tsx`'s own doc
 *    comment gives (no analytics-event SDK exists anywhere in this app).
 *  - `useToolkitsDetailsQuery` (RTK Query, single-toolkit GET) is replaced
 *    with this unit's own `useToolkitDetail` (`../api/toolkits.ts`) — no
 *    generated GET-single endpoint exists either; that hook derives detail
 *    from the real `listToolkitInstances` collection client-side (see its
 *    own doc comment).
 *
 * Split into this file (the public `ToolkitEditor` component) and
 * `./ToolkitEditorParts.tsx` (the prop-shape types, the state hook, the
 * body/save-button sub-components) purely to stay under the §3.5 400-line
 * file-length budget — same `PipelineEditorParts.tsx` precedent this batch
 * already established for the sibling pipelines editor. The prop-shape
 * types live in `ToolkitEditorParts.tsx`, not here, so the dependency edge
 * between the two files stays one-directional (this file imports FROM
 * `ToolkitEditorParts.tsx` only, never the reverse) — the same "no-circular"
 * (R-L2) reason `PipelineEditor.tsx` keeps `PipelineEditorDeps`/
 * `PipelineEditorShellProps` in `PipelineEditorParts.tsx` too; the type
 * re-export below (`export type {...} from './ToolkitEditorParts'`) keeps
 * every external caller's import path (`from './ToolkitEditor'` or
 * `features/toolkits`' own `index.ts`, which imports every one of these
 * types from THIS file) unchanged. `ToolkitEditorParts.tsx` has no OTHER
 * caller — this file is that split's entire reason to exist.
 */
export function ToolkitEditor({ toolkit, isVisible, onCloseToolkitEditor, onToolkitCreated, onToolkitUpdated, deps }: ToolkitEditorProps): ReactNode {
  const state = useToolkitEditorState(toolkit ?? EMPTY_PARTICIPANT, isVisible);
  /**
   * #613. Both save buttons below accept an `onError` and NOTHING in the app
   * supplied one, so a server refusal — including the new per-field credential
   * refusal — vanished silently. This turns it into the field errors the form
   * already knows how to paint.
   */
  const { toolkitValidation, reportSaveError, clearSaveErrors } = useToolkitSaveValidation();
  // #2016: a Save click is always answered in words (saved / blocked / refused).
  const notice = useToolkitEditorNotice();
  const { failed: noticeFailed, saved: noticeSaved } = notice;

  const { isDirty, setIsDirty, validationState, setValidationState, revertCredentialsRef, isCreating, isMCP, projectId, toolkitId, scopedProjectId, isPublic, isError, editToolDetail, formInitialValues, setFormInitialValues, handleChangeToolDetail, handleDiscard } = state;

  /**
   * The two real save mutations, wrapped so a rejection is RECORDED on its way
   * past. `CreateToolkitButton`/`SaveToolkitButton` each catch their own
   * rejection into an `onError` that nothing in this app ever supplied, so a
   * refused save — including the new per-field credential refusal — vanished
   * entirely. Wrapping here rather than adding a third `onError` prop keeps
   * `ToolkitEditorParts.tsx` inside its §3.5 400-line budget; the rethrow keeps
   * each button's own failure handling unchanged.
   */
  const depsWithSaveErrors = useMemo(
    () => ({
      ...deps,
      createToolkit: async (args: Parameters<UseToolkitCreateMutation>[0]) => {
        try {
          return await deps.createToolkit(args);
        } catch (error) {
          noticeFailed(error, reportSaveError(error));
          throw error;
        }
      },
      saveToolkit: async (args: Parameters<UseToolkitEditMutation>[0]) => {
        try {
          return await deps.saveToolkit(args);
        } catch (error) {
          noticeFailed(error, reportSaveError(error));
          throw error;
        }
      },
    }),
    [deps, reportSaveError, noticeFailed],
  );

  /**
   * Any real edit drops the recorded refusal. Both save buttons gate on
   * `validationState.hasErrors`, which the server errors feed, so without this
   * the gate would stay latched shut on an error the user has already fixed.
   */
  const handleEditAndClearErrors = useCallback(
    (updater: (prev: ToolkitFormEditDetail | null) => ToolkitFormEditDetail | null, options?: { readonly isAutoSelect?: boolean }) => {
      handleChangeToolDetail(updater, options);
      clearSaveErrors();
    },
    [handleChangeToolDetail, clearSaveErrors],
  );

  const handleClose = useCallback(() => {
    onCloseToolkitEditor?.();
    deps.onEditorClosed?.();
  }, [onCloseToolkitEditor, deps]);

  const handleToolkitCreated = useCallback((result: ToolkitWriteResult) => onToolkitCreated?.(result), [onToolkitCreated]);

  const handleToolkitSaved = useCallback(
    (_result: ToolkitWriteResult, toolkitData: ToolkitFormValues) => {
      if (!isCreating && toolkit) {
        const updated: ToolkitEditorParticipant = {
          ...toolkit,
          entity_meta: { ...toolkit.entity_meta, name: toolkitData.name },
          meta: { ...toolkit.meta, name: toolkitData.name },
        };
        onToolkitUpdated?.(updated, true);
      }
      setIsDirty(false);
      if (!isCreating) noticeSaved();
    },
    [isCreating, toolkit, onToolkitUpdated, setIsDirty, noticeSaved],
  );

  if (!toolkit) return null;

  return deps.renderShell({
    isVisible,
    isDirty,
    onClose: handleClose,
    title: resolveToolkitName(isCreating, isMCP, editToolDetail, toolkit),
    onDiscard: handleDiscard,
    error: isError,
    saveButton: (
      <ToolkitEditorSaveButton
        isCreating={isCreating}
        editToolDetail={editToolDetail}
        toolkitId={toolkitId}
        isDirty={isDirty}
        validationState={validationState}
        createProjectId={projectId}
        saveProjectId={scopedProjectId}
        deps={depsWithSaveErrors}
        onToolkitCreated={handleToolkitCreated}
        onToolkitSaved={handleToolkitSaved}
        onBlocked={notice.blocked}
      />
    ),
    ...(editToolDetail === null ? { contentSx: emptyContentSx } : {}),
    children: (
      <>
        {notice.view}
        <ToolkitEditorBody
          isCreating={isCreating}
          isMCP={isMCP}
          editToolDetail={editToolDetail}
          onChangeToolDetail={handleEditAndClearErrors}
          formInitialValues={formInitialValues}
          setFormInitialValues={setFormInitialValues}
          disabled={resolveToolkitFormDisabled(isPublic, deps.hasPublicProjectAccess)}
          projectId={scopedProjectId}
          revertCredentialsRef={revertCredentialsRef}
          onValidationStateChange={setValidationState}
          toolkitValidation={toolkitValidation}
        />
      </>
    ),
  });
}
