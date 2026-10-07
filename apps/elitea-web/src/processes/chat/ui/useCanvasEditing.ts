/**
 * The canvas editor's open/close state AND its save — the composition point
 * `features/chat-messages`' `CanvasEditor` never had.
 *
 * `ChatWithEditors.hooks.ts` supplied `onShowCanvasEditor` as a documented
 * no-op and a `canvasEditorRef` whose `.current` was permanently `null`,
 * "because CanvasEditor.tsx exists but has NO established composition point
 * anywhere in this app yet". Two things were broken by that, not one:
 *
 *  1. opening a canvas from a chat message did nothing at all;
 *  2. `useEditorMutex`'s `closeHandlers.isEditingCanvas` — the branch that
 *     SAVES the open canvas before another editor takes its place — resolved
 *     to `null?.save?.()` and silently discarded the user's edits.
 *
 * This hook is the real thing: it holds the selected code block, drives the
 * shared `isEditingCanvas` flag (so the mutex and the nav blocker can both
 * see it), and hands back the ref the mutex needs.
 *
 * # The third break, closed here: the edits went nowhere
 *
 * `CanvasEditor` closes by calling `onCloseCanvasEditor(hasChange,
 * finalResult, language)` — the live document, deliberately read out of the
 * editor rather than off its debounced state. This hook took NONE of those
 * three arguments and made no request at all, so every keystroke was dropped
 * the moment the drawer closed. Both halves were correct on their own; the
 * wiring between them was the whole defect, which is the shape this codebase
 * keeps meeting.
 *
 * The save PUTs `/elitea_core/canvas/prompt_lib/{p}/{canvasUUID}` — the route
 * the canvas editor's own REST client already had a fetcher for
 * (`entities/canvas`'s `useEditCanvasMutation`) and no caller. It is made
 * HERE, inside the hook, rather than injected by the composition root: an
 * optional dependency the root can forget is exactly how the previous version
 * of this file became a no-op.
 *
 * Two guards on it, and both matter:
 *
 *  * NOTHING IS SAVED WITHOUT A `canvasId`. A block the user opened before
 *    the create call came back has no server-side canvas to write to, and a
 *    PUT to an undefined id is a 404 that would surface as a lost edit.
 *  * NOTHING IS SAVED WITHOUT A CHANGE. `hasChange` mirrors the editor's own
 *    undo state; a language switch counts as a change of its own, because the
 *    language is stored on the version too.
 */
import { useCallback, useEffect, useRef, useState } from 'react';

import { useEditCanvasMutation } from '@/entities/canvas';
import type { CanvasEditorHandle } from '@/features/chat-messages';
import { EliteaApiError } from '@/shared/api/generated/mutator';
import { t } from '@/shared/i18n';
import { trackCanvasSave } from '@/shared/lib/canvasSaveGate';
import { useEditorStateStore } from '@/shared/lib/editorState';
import { useSelectedProject } from '@/widgets/app-shell';

import type { CanvasEditPayload } from '../model/useEditorMutex';

/** What `CanvasEditor` needs about the block being edited — `CanvasEditPayload`, narrowed from `unknown`. */
export interface SelectedCodeBlockInfo {
  readonly codeBlock: string;
  readonly language: string;
  readonly isBlock: boolean;
  readonly canvasId?: string;
  readonly messageItemId?: string | number;
  readonly viewOnly?: boolean;
}

export interface UseCanvasEditingResult {
  readonly isEditingCanvas: boolean;
  readonly selectedCodeBlockInfo: SelectedCodeBlockInfo | undefined;
  readonly canvasEditorRef: React.RefObject<CanvasEditorHandle | null>;
  readonly onShowCanvasEditor: (payload: CanvasEditPayload) => void;
  /**
   * `CanvasEditor`'s own close contract: `(hasChange, finalResult, language)`.
   * Every argument is optional so a caller with nothing to save — the drawer's
   * backdrop, the nav blocker — can close with no arguments at all.
   */
  readonly onCloseCanvasEditor: (hasChange?: boolean, finalResult?: string, language?: string) => void;
  /** The project-scoped save the editor's language picker calls directly. */
  readonly editCanvas: (params: { projectId: string | number; canvasUUID: string; name?: string; canvas_type?: string; code_language?: string }) => Promise<unknown>;
  /** The project the save writes into; `undefined` until one is selected. */
  readonly projectId: string | undefined;
  /** True while the closing save is in flight; the drawer stays open until it lands. */
  readonly isSaving: boolean;
  /**
   * Why the last closing save failed, in words the server chose for a user
   * (`safe_message`), or `undefined`. While it is set the editor stays open
   * with the user's text in it — nothing is dropped.
   */
  readonly saveError: string | undefined;
  /** Closes WITHOUT saving — the explicit way out of a save that keeps failing. */
  readonly discardAndClose: () => void;
  /** The canvas the editor last closed on, so its block's control can take focus back. */
  readonly lastClosedCanvasId: string | undefined;
}

/**
 * The server's own words for a refused canvas write, or `undefined`. A
 * canvas route's `safe_message` (a 413 `canvas_too_large` names the size and
 * the limit) is meant for a user and is shown as is; anything else — a
 * transport failure, a body without one — is left to the caller's generic
 * sentence rather than surfacing a transport string. Shared by the save here
 * and the create in `./useCanvasCreation.ts`, which hit the same cap.
 */
export function canvasRefusalMessage(error: unknown): string | undefined {
  if (!(error instanceof EliteaApiError) || error.failure.kind !== 'http') return undefined;
  const body = error.failure.body;
  if (typeof body !== 'object' || body === null) return undefined;
  const safe = (body as { readonly safe_message?: unknown }).safe_message;
  return typeof safe === 'string' && safe.trim() !== '' ? safe : undefined;
}

/** The words to show for a failed save: the server's, else a generic sentence. */
function canvasSaveErrorMessage(error: unknown): string {
  return (
    canvasRefusalMessage(error) ??
    t('processes.chat.canvas.saveFailed', 'The document could not be saved. Your changes are still here — try again.')
  );
}

/**
 * `CanvasEditPayload` types every field as `unknown` (it crosses a
 * `features` boundary as opaque data), so each one is narrowed here rather
 * than cast wholesale. A payload with no code block opens nothing: the
 * editor's own first guard already renders `display: none` for that case,
 * and flipping `isEditingCanvas` for an invisible editor would wedge the
 * mutex — every other editor would then queue behind a canvas nobody can
 * see or close.
 */
export function toSelectedCodeBlockInfo(payload: CanvasEditPayload): SelectedCodeBlockInfo | undefined {
  const codeBlock = typeof payload.codeBlock === 'string' ? payload.codeBlock : undefined;
  if (codeBlock === undefined || codeBlock === '') return undefined;
  return {
    codeBlock,
    language: typeof payload.language === 'string' ? payload.language : 'markdown',
    isBlock: payload.isBlock === true,
    ...(typeof payload.canvasId === 'string' ? { canvasId: payload.canvasId } : {}),
    ...(typeof payload.messageItemId === 'string' || typeof payload.messageItemId === 'number'
      ? { messageItemId: payload.messageItemId }
      : {}),
    ...(payload.viewOnly === true ? { viewOnly: true } : {}),
  };
}

/**
 * Whether a close carries an edit worth a PUT: never for a read-only canvas,
 * and otherwise when the text changed or the language did (the language is
 * stored on the version too).
 */
function closeNeedsSave(open: SelectedCodeBlockInfo | undefined, hasChange: boolean | undefined, language: string | undefined): boolean {
  if (open?.viewOnly === true) return false;
  const languageChanged = typeof language === 'string' && language !== open?.language;
  return hasChange === true || languageChanged;
}

export function useCanvasEditing(): UseCanvasEditingResult {
  const isEditingCanvas = useEditorStateStore((s) => s.isEditingCanvas);
  const setEditingCanvas = useEditorStateStore((s) => s.setEditingCanvas);
  const [selectedCodeBlockInfo, setSelectedCodeBlockInfo] = useState<SelectedCodeBlockInfo | undefined>(undefined);
  const canvasEditorRef = useRef<CanvasEditorHandle | null>(null);
  const { project } = useSelectedProject();
  const projectId = project?.id === undefined ? undefined : String(project.id);
  const { mutateAsync: saveCanvas } = useEditCanvasMutation();

  /*
   * The open block, read LIVE at close time rather than captured.
   *
   * `useEditorMutex` keeps `onCloseCanvasEditor` behind an imperative handle
   * and calls it a whole editor session later, so a callback that closed over
   * the block selected when it was BUILT would save the previous canvas's id
   * — the same closure-staleness that made the conversation pin write to the
   * wrong row (`usePinConversation.hooks.ts`).
   */
  const openBlockRef = useRef<SelectedCodeBlockInfo | undefined>(undefined);
  useEffect(() => {
    openBlockRef.current = selectedCodeBlockInfo;
  }, [selectedCodeBlockInfo]);
  const projectIdRef = useRef(projectId);
  projectIdRef.current = projectId;

  const editCanvas = useCallback(
    async (params: { projectId: string | number; canvasUUID: string; name?: string; canvas_type?: string; code_language?: string }) =>
      saveCanvas(params),
    [saveCanvas],
  );

  const onShowCanvasEditor = useCallback(
    (payload: CanvasEditPayload) => {
      const info = toSelectedCodeBlockInfo(payload);
      if (info === undefined) return;
      setSelectedCodeBlockInfo(info);
      setEditingCanvas(true);
    },
    [setEditingCanvas],
  );

  const [isSaving, setIsSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | undefined>(undefined);
  const [lastClosedCanvasId, setLastClosedCanvasId] = useState<string | undefined>(undefined);
  /** Read synchronously: a second Escape while the first save is in flight must not start another. */
  const savingRef = useRef(false);

  const closeNow = useCallback(() => {
    setLastClosedCanvasId(openBlockRef.current?.canvasId);
    setSaveError(undefined);
    setEditingCanvas(false);
    setSelectedCodeBlockInfo(undefined);
  }, [setEditingCanvas]);

  /*
   * The close WAITS for the save, and a failed save keeps the editor open.
   *
   * This was fire-and-forget with the failure swallowed: the drawer closed at
   * once and a refused PUT — a canvas over the server's size cap answers 413
   * — dropped the user's edit without a word. Now the drawer stays open while
   * the save is in flight (so the composer behind its modal cannot send a
   * turn that reads the previous version), closes when it lands, and on a
   * failure stays open with the text intact and the server's reason shown
   * beside it; `discardAndClose` is the explicit way out.
   *
   * The save is also registered with `canvasSaveGate`, which the send path
   * awaits — the belt to the modal's braces, for a close that does not go
   * through this drawer (the editor mutex swapping the canvas for another
   * editor).
   */
  const onCloseCanvasEditor = useCallback(
    (hasChange?: boolean, finalResult?: string, language?: string) => {
      if (savingRef.current) return;
      const canvasUUID = openBlockRef.current?.canvasId;
      const currentProjectId = projectIdRef.current;
      if (
        canvasUUID === undefined ||
        currentProjectId === undefined ||
        typeof finalResult !== 'string' ||
        !closeNeedsSave(openBlockRef.current, hasChange, language)
      ) {
        closeNow();
        return;
      }
      savingRef.current = true;
      setIsSaving(true);
      setSaveError(undefined);
      const save = saveCanvas({
        projectId: currentProjectId,
        canvasUUID,
        canvas_content: finalResult,
        ...(typeof language === 'string' ? { code_language: language } : {}),
      });
      trackCanvasSave(save);
      save.then(
        () => {
          savingRef.current = false;
          setIsSaving(false);
          // Only if the canvas that was saved is still the one open.
          if (openBlockRef.current?.canvasId === canvasUUID) closeNow();
        },
        (error: unknown) => {
          savingRef.current = false;
          setIsSaving(false);
          setSaveError(canvasSaveErrorMessage(error));
        },
      );
    },
    [closeNow, saveCanvas],
  );

  const discardAndClose = useCallback(() => {
    if (savingRef.current) return;
    closeNow();
  }, [closeNow]);

  return {
    isEditingCanvas,
    selectedCodeBlockInfo,
    canvasEditorRef,
    onShowCanvasEditor,
    onCloseCanvasEditor,
    editCanvas,
    projectId,
    isSaving,
    saveError,
    discardAndClose,
    lastClosedCanvasId,
  };
}
