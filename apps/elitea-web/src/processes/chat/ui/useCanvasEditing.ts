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

  const onCloseCanvasEditor = useCallback(
    (hasChange?: boolean, finalResult?: string, language?: string) => {
      const open = openBlockRef.current;
      const canvasUUID = open?.canvasId;
      const currentProjectId = projectIdRef.current;
      const languageChanged = typeof language === 'string' && language !== open?.language;
      if (
        canvasUUID !== undefined &&
        currentProjectId !== undefined &&
        typeof finalResult === 'string' &&
        open?.viewOnly !== true &&
        (hasChange === true || languageChanged)
      ) {
        // Fire-and-forget, and a failure is swallowed HERE only because this
        // app has no toast hook at this layer yet (the same statement
        // `CanvasEditor`'s own `onError` prop carries). The request itself is
        // what was missing; reporting its failure is a smaller, separate gap
        // and is recorded as one rather than implied.
        void saveCanvas({
          projectId: currentProjectId,
          canvasUUID,
          canvas_content: finalResult,
          ...(typeof language === 'string' ? { code_language: language } : {}),
        }).catch(() => undefined);
      }
      setEditingCanvas(false);
      setSelectedCodeBlockInfo(undefined);
    },
    [saveCanvas, setEditingCanvas],
  );

  return { isEditingCanvas, selectedCodeBlockInfo, canvasEditorRef, onShowCanvasEditor, onCloseCanvasEditor, editCanvas, projectId };
}
