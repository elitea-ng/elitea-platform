/**
 * ui/canvas/CanvasEditor.tsx — full canvas editor panel with code/markdown
 * table/mermaid editor, sync toolbar, and split-view for diagrams, ported
 * from `apps/elitea-ui/src/pages/NewChat/CanvasEditor.jsx` (C4 batch).
 *
 * This is the main editor surface: a code mirror (or table editor, or
 * mermaid split-view) with undo/redo, language select, copy, save, and
 * real-time sync via canvas socket events.
 *
 * **DEVIATIONS (disclosed):**
 *  1. Redux `useSelector` for the current user name → taken as an explicit
 *     `viewer` parameter (baseline resolves it internally via
 *     `useSelectedProjectId`/Redux store; this port exposes it as an input
 *     so `entities/`-level code never depends on a "page-level" hook). It
 *     carries the principal ID as well as the name, because the presence
 *     roster is keyed by id and a name alone cannot recognise this tab's own
 *     entry — see `viewer` and the presence block below.
 *  2. RTK Query mutations (`useEditCanvasMutation`, `useListModelsQuery`,
 *     `useGenerateContentBlockingMutation`) → replaced with injected plain
 *     async fetchers (`editCanvas`, `generateQuickFix`). The `entities/canvas`
 *     API hooks exist but the editor's own use of mutation error display is
 *     surfaced as an `onError` callback for the feature layer to decide how
 *     to present (toast vs inline error).
 *  3. GA telemetry (`useTrackEvent`, `GA_EVENT_NAMES`, `GA_EVENT_PARAMS`) →
 *     dropped entirely (the new app has no GA integration yet).
 *  4. `react-split` for the mermaid split-view → replaced with a simple
 *     CSS flex layout (the split/resize UX is not worth the dependency for
 *     a prototype). The reference's FULL-SCREEN mode, which that pane was
 *     bundled with, IS ported — as its own header control that re-anchors
 *     this element to the viewport, with Escape to collapse (see
 *     `isFullScreen` below and `../../lib/canvasEditorKeys`).
 *  5. `useLanguageLinter` → replaced with `extensions` and `onChangeLanguage`
 *     injected from the feature layer (the linter integration depends on the
 *     editor's view instance, which the feature layer owns).
 *  6. Editor presence is WIRED as of #622, over the project SSE plane rather
 *     than over `chat_canvas_editors_change` — see `presence` below and
 *     `../../model/useCanvasPresence.ts`. What is still true, and still
 *     disclosed by `concurrentEditNotice`: presence is not a LOCK. The server
 *     refuses no write on the roster, so two people who both hold the canvas
 *     are still last-write-wins; the roster is what stops the second person
 *     from silently typing over the first.
 *  7. The baseline's five mermaid quick-fix toasts are GONE. Three of them
 *     fire on every click in a default install, where
 *     `ELITEA_CONFIGURATIONS_ENABLED` is false and neither the model nor the
 *     `MERMAID_QUICK_FIX` service prompt is reachable. The control is gated on
 *     the capability instead — `./MermaidQuickFixButton.tsx` renders nothing
 *     when it cannot run. Genuine runtime failures reach `onError`.
 *  8. The table's csv/xlsx export IS ported, and without the reference's two
 *     dependencies. `excellentexport` scraped a rendered `<table>` out of the
 *     DOM; `../../lib/tableExport` writes both files from the table's own
 *     model instead — which is also the only correct source, since a
 *     virtualised grid does not hold every row in the DOM. The reference's
 *     `SplitButton` becomes a button that opens a menu of the two formats
 *     (`./table/CanvasTableControls.tsx`), because a control that both fires a
 *     hidden default and opens a menu is the harder half to reach.
 *
 * ── TWO EDITORS, ONE HEADER ────────────────────────────────────────────────
 * A canvas is edited by ONE of two panes: `CodeMirrorEditor` (code and the
 * mermaid source) or `MarkdownTableEditor` (a `markdownTable` canvas). Both
 * expose the same `undo`/`redo`/`getCode` handle shape, and the header's
 * undo/redo/copy/close must dispatch to whichever is mounted — see
 * `activeEditor` below. Their undo depths are tracked separately
 * (`codeHistory`/`tableHistory`) because they are separate histories:
 * switching the language switches which pane is mounted, and the header must
 * then reflect THAT pane's depth, not the one that just unmounted.
 */
import { forwardRef, lazy, Suspense, useCallback, useEffect, useImperativeHandle, useMemo, useRef, useState } from 'react';

import { Avatar, Box, Tooltip, Typography } from '@mui/material';

import { useCanvasDetailSocket, useCanvasEditSocket, useCanvasErrorSocket, useCanvasSyncSocket } from '@/entities/canvas/api/canvasSocket';
import { useCanvasRoom } from '@/shared/api/socket/rooms';
import { t } from '@/shared/i18n';
import { getInitials, stringToColor } from '@/shared/lib/string';
import type { CodeMirrorEditorHandle } from '@/shared/ui/CodeMirrorEditor';
import { CodeMirrorEditor } from '@/shared/ui/CodeMirrorEditor';
import { MermaidDiagram } from '@/shared/ui/MermaidDiagram';

import type { MarkdownTableData } from '../../lib/markdownTable';
import { parseMarkdownTable } from '../../lib/markdownTable';
import { useCanvasPresence } from '../../model/useCanvasPresence';
import type { UseMermaidQuickFixResult } from '../../model/useMermaidQuickFix';

import { canvasEditorKeyAction } from '../../lib/canvasEditorKeys';
import { exportTable } from '../../lib/tableExport';
import type { TableExportFormat } from '../../lib/tableExport';

import { getCanvasCodeExtensions } from './canvasCodeExtensions';

import { extraCodeFromBlock } from './Canvas';
import { CanvasEditHeader } from './CanvasEditHeader';
import { MermaidQuickFixButton } from './MermaidQuickFixButton';
import { SaveToArtifactsDialog } from './SaveToArtifactsDialog';
import type { MarkdownTableEditorHandle } from './table/MarkdownTableEditor';
import { MarkdownTableEditor } from './table/MarkdownTableEditor';

import type { CanvasFileSource } from '../../lib/canvasFileSource';
import type { DocumentEditorHandle } from './DocumentEditor';

/**
 * The `document` pane (issue #879), lazy-loaded — see `./DocumentEditor.tsx`'s
 * own module doc for why: it pulls in ProseMirror's whole editing engine, and
 * the code/table/mermaid path (still the common case) must not pay for that
 * chunk. `import('./DocumentEditor')`'s default export is the component
 * itself (that file exports both named and default for exactly this).
 */
const LazyDocumentEditor = lazy(() => import('./DocumentEditor'));

/**
 * How long the editor coalesces keystrokes before broadcasting the document to
 * the other editors. See `notifyChange` for why it is not the reference's 30 ms.
 */
const REMOTE_EDIT_DEBOUNCE_MS = 400;

/** The reference's MAX_NUMBER_AVATARS_SHOWN (AuthorContainer.jsx). */
const MAX_PRESENCE_AVATARS = 3;

export interface CanvasEditorProps {
  /** Info about the selected code block this editor is editing. */
  readonly selectedCodeBlockInfo?: {
    readonly codeBlock: string;
    readonly language: string;
    readonly isBlock: boolean;
    readonly canvasId?: string;
    readonly messageItemId?: string | number;
    readonly viewOnly?: boolean;
    readonly isCreatingCanvas?: boolean;
    readonly createCanvasError?: unknown;
  };
  /** Called when the editor is closed — `(hasChange, finalResult, language)` where `hasChange` mirrors whether the user had undo-pending changes. */
  readonly onCloseCanvasEditor: (hasChange: boolean, finalResult: string, language: string) => void;
  /** Called when the user requests regeneration (whole-message mode only). */
  readonly onRegenerate?: () => void;
  /** Called when the user requests deletion (whole-message mode only). */
  readonly onDelete?: () => void;
  /** Interaction UUID for tracking. */
  readonly interaction_uuid?: string;
  /** Conversation UUID for tracking. */
  readonly conversation_uuid?: string;
  /** When true, no editing actions are available. */
  readonly viewOnly?: boolean;
  /**
   * Who is looking. Both halves feed the presence read-only rule, and `id` is
   * the one that decides it: the roster the server answers every beat with
   * contains this very tab, and an editor that cannot recognise its own entry
   * reads it as a stranger holding the canvas and goes read-only against the
   * only person editing. `name` is the fallback for a roster with no id, and
   * is what the "X is editing…" notice renders.
   *
   * One object rather than two props to stay inside the §3.5 12-prop budget.
   */
  readonly viewer?: { readonly id?: string | undefined; readonly name?: string | undefined };
  /**
   * The mermaid quick-fix capability + runner, from
   * `useMermaidQuickFix({ projectId, readOnly })`. Omit it — or pass one
   * reporting `isAvailable: false` — and NO quick-fix control is rendered.
   * Grouped as one object rather than two props to stay inside the §3.5
   * 12-prop component budget.
   */
  readonly quickFix?: UseMermaidQuickFixResult;
  /** Surfaces editor-level failures (canvas socket error, failed quick-fix, failed CSV import, failed clipboard write). This app has no toast hook yet. */
  readonly onError?: (error: unknown) => void;
  /** Plain async fetcher for editing an existing canvas (baseline: `useEditCanvasMutation().mutate`). */
  readonly editCanvas?: (params: { projectId: string | number; canvasUUID: string; name?: string; canvas_type?: string; code_language?: string }) => Promise<unknown>;
  /** Project ID for canvas edits. */
  readonly projectId?: string | number;
  /**
   * "Save to artifacts" (issue #878) — grouped to stay within the §3.5
   * prop budget (recorded in `scripts/lib/budgets-core.mjs`'s waiver list
   * alongside this file). Omitted, the header offers no such control.
   */
  readonly saveToArtifacts?: {
    /** Where this canvas was opened from / last saved to, if either happened — pre-fills the dialog and lets a plain re-save skip the overwrite confirm. */
    readonly source?: CanvasFileSource;
    /** A filename to suggest when there is no `source` yet. */
    readonly suggestedName?: string;
    readonly onSaved: (source: CanvasFileSource) => void;
  };
}

export interface CanvasEditorHandle {
  /** Saves the current editor content and fires `onCloseCanvasEditor`. */
  save: () => void;
}

/**
 * Renders the full canvas editor panel.
 *
 * Matches the baseline `CanvasEditor.jsx` structure:
 * - Header row with close, undo/redo, copy, regenerate, delete, language select
 * - Code mirror editor (or markdown table editor) for the content
 * - Mermaid split-view (code + rendered preview) for diagram canvases
 * - Real-time sync: joins the canvas socket room on mount, listens for
 *   sync/detail/error/editors-change events, leaves the room on unmount
 * - Loading/error states for new canvas creation
 */
export const CanvasEditor = forwardRef<CanvasEditorHandle, CanvasEditorProps>(
  function CanvasEditor(
    {
      selectedCodeBlockInfo,
      onCloseCanvasEditor,
      onRegenerate,
      onDelete,
      interaction_uuid,
      conversation_uuid,
      viewOnly = false,
      viewer,
      quickFix,
      onError,
      editCanvas,
      projectId,
      saveToArtifacts,
    },
    ref,
  ) {
    const [code, setCode] = useState(selectedCodeBlockInfo?.codeBlock ?? '');
    const [readOnly, setReadOnly] = useState(viewOnly);
    const [codeLanguage, setCodeLanguage] = useState(selectedCodeBlockInfo?.language ?? 'markdown');
    /** "Save to artifacts" dialog (issue #878) — a session-local UI flag, same rationale as `isFullScreen` below: it belongs to THIS reading, not to the canvas. */
    const [saveDialogOpen, setSaveDialogOpen] = useState(false);

    /**
     * Which pane is mounted. A `markdownTable` canvas renders the table
     * editor, a `document` canvas (issue #879) renders the rich-text pane,
     * and everything else (including `mermaid`, whose source pane is a code
     * editor) renders CodeMirror.
     */
    const isTableEditing = codeLanguage === 'markdownTable';
    const isDocumentEditing = codeLanguage === 'document';

    /** The CodeMirror host's imperative handle. Null while another pane is mounted. */
    const editorRef = useRef<CodeMirrorEditorHandle>(null);
    /** The table editor's imperative handle. Null while another pane is mounted. */
    const tableRef = useRef<MarkdownTableEditorHandle>(null);
    /** The document (rich-text) editor's imperative handle — issue #879. Null while another pane is mounted. */
    const docRef = useRef<DocumentEditorHandle>(null);

    /*
     * Three histories, not one. Each pane owns its own undo stack —
     * CodeMirror's and the document pane's are the document's own, the
     * table's is a snapshot list — and only the mounted one can answer an
     * undo. Keeping a single `canUndo` would leave the header enabled from a
     * pane that just unmounted.
     */
    const [codeHistory, setCodeHistory] = useState({ canUndo: false, canRedo: false });
    const [tableHistory, setTableHistory] = useState({ canUndo: false, canRedo: false });
    const [docHistory, setDocHistory] = useState({ canUndo: false, canRedo: false });
    const { canUndo, canRedo } = isTableEditing ? tableHistory : isDocumentEditing ? docHistory : codeHistory;

    /*
     * Stable identity (each pane installs its depth listener keyed on this
     * object), and — load-bearing — each setter BAILS OUT when the flag has not
     * actually changed.
     *
     * `(prev) => ({ ...prev, canUndo: value })` returns a new object every
     * call, so React never skips the re-render even when nothing changed. Both
     * panes report their depth on every document change, that re-render feeds
     * `value={code}` back into the editor, which reports depth again — typing
     * into a canvas wedged the tab. Returning `prev` unchanged is what stops
     * it; `./CanvasEditor.table.test.tsx`'s "the code pane" cases hang without
     * this and pass with it.
     */
    const historyCallbacks = useMemo(
      () => ({
        onCanUndo: (value: boolean) =>
          setCodeHistory((prev) => (prev.canUndo === value ? prev : { ...prev, canUndo: value })),
        onCanRedo: (value: boolean) =>
          setCodeHistory((prev) => (prev.canRedo === value ? prev : { ...prev, canRedo: value })),
      }),
      [],
    );
    /** Same, for the table pane — its model hook holds these in a `useCallback` dep list. */
    const tableHistoryCallbacks = useMemo(
      () => ({
        onCanUndo: (value: boolean) =>
          setTableHistory((prev) => (prev.canUndo === value ? prev : { ...prev, canUndo: value })),
        onCanRedo: (value: boolean) =>
          setTableHistory((prev) => (prev.canRedo === value ? prev : { ...prev, canRedo: value })),
      }),
      [],
    );
    /** Same, for the document pane (issue #879). */
    const docHistoryCallbacks = useMemo(
      () => ({
        onCanUndo: (value: boolean) =>
          setDocHistory((prev) => (prev.canUndo === value ? prev : { ...prev, canUndo: value })),
        onCanRedo: (value: boolean) =>
          setDocHistory((prev) => (prev.canRedo === value ? prev : { ...prev, canRedo: value })),
      }),
      [],
    );

    /*
     * Memoised on the language, NOT rebuilt per render.
     *
     * `getCanvasCodeExtensions` returns a fresh array on every call (`[]` for
     * the 42 languages with no package installed), and
     * `@uiw/react-codemirror` compares `extensions` by REFERENCE — so the
     * inline call this replaced made the editor reconfigure its whole
     * extension set on every keystroke. That was wasteful rather than fatal
     * (the wedge above was the history setters), but it is a per-keystroke
     * rebuild of the editor for no change in what the editor holds.
     */
    const codeExtensions = useMemo(() => getCanvasCodeExtensions(codeLanguage), [codeLanguage]);

    /**
     * The pane the header's buttons act on. Both handles expose the same
     * `undo`/`redo`/`getCode`, so the header does not branch — this does.
     */
    const activeEditor = useCallback(
      (): Pick<CodeMirrorEditorHandle, 'undo' | 'redo' | 'getCode'> | null =>
        isTableEditing ? tableRef.current : isDocumentEditing ? docRef.current : editorRef.current,
      [isTableEditing, isDocumentEditing],
    );

    const [hasSelectedRowsColumns, setHasSelectedRowsColumns] = useState({
      hasSelectedRows: false,
      hasSelectedColumns: false,
    });

    const { sendChangeToRemote } = useCanvasEditSocket();

    /*
     * THE CANVAS ROOM. `useCanvasRoom` had ZERO consumers, so this editor was
     * emitting `chat_canvas_edit` into a room it had never joined (#622). The
     * hook is reference-counted and leaves on unmount, which is also the
     * "leaveTheCanvasRoom" TODO that used to sit further down this file.
     *
     * It is the SOCKET half and stays dormant wherever `VITE_SOCKET_SERVER` is
     * empty — the no-op client makes every emit a no-op. Presence does NOT ride
     * on it; that is the SSE half below.
     */
    useCanvasRoom(selectedCodeBlockInfo?.canvasId, {
      ...(projectId === undefined ? {} : { projectId: String(projectId) }),
      enabled: !!selectedCodeBlockInfo?.canvasId,
    });

    /**
     * The baseline's `notifyChange`: keep local state, then broadcast.
     *
     * LOCAL STATE IS UPDATED ON EVERY KEYSTROKE and the BROADCAST IS DEBOUNCED.
     * The two halves are separated deliberately: `code` is what Save and Copy
     * read, so delaying it would hand the caller the previous revision, while
     * the broadcast carries the WHOLE document every time and is what a
     * per-keystroke cadence actually costs.
     *
     * The reference debounces at 30 ms
     * (apps/elitea-ui/.../useCodeMirror.hooks.js), which at sustained typing is
     * one full-document emit every 30 ms per editor, with no server-side
     * throttle and no rate limit. That number is recorded rather than copied:
     * it was a socket emit into a room, and this is a coalescing window for a
     * shared document. 400 ms is one emit per typing pause instead of ~13 per
     * second, and is below the threshold at which a collaborator perceives the
     * other cursor as stalled.
     */
    const remoteEmitTimerRef = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
    const notifyChange = useCallback(
      (newCode: string) => {
        setCode((current) => {
          if (current === newCode) return current;
          const canvasId = selectedCodeBlockInfo?.canvasId;
          if (canvasId) {
            if (remoteEmitTimerRef.current !== undefined) clearTimeout(remoteEmitTimerRef.current);
            remoteEmitTimerRef.current = setTimeout(() => {
              remoteEmitTimerRef.current = undefined;
              sendChangeToRemote(canvasId, newCode);
            }, REMOTE_EDIT_DEBOUNCE_MS);
          }
          return newCode;
        });
      },
      [selectedCodeBlockInfo?.canvasId, sendChangeToRemote],
    );

    /** Replaces the whole table — the header's CSV/TSV import and canvas sync both land here. */
    const onImportTableData = useCallback((data: MarkdownTableData) => {
      tableRef.current?.resetTable(data);
    }, []);

    // Canvas sync — when another editor pushes content, update local state
    const onCanvasSync = useCallback(
      (newContent: unknown) => {
        const extracted = extraCodeFromBlock(newContent as string);
        if (code !== extracted) {
          setCode(extracted);
          // Push it into the live document too: `code` is passed as the
          // editor's `value`, but whichever pane is mounted owns the doc, so a
          // state update alone would leave the visible content stale.
          if (isTableEditing) {
            onImportTableData(parseMarkdownTable(extracted));
          } else if (isDocumentEditing) {
            docRef.current?.setCode(extracted);
          } else {
            editorRef.current?.setCode(extracted);
          }
        }
      },
      [code, isTableEditing, isDocumentEditing, onImportTableData],
    );

    const { listenCanvasSyncEvent, stopListenCanvasSyncEvent } = useCanvasSyncSocket({ onCanvasSync });
    const { listenCanvasDetailEvent, stopListenCanvasDetailEvent } = useCanvasDetailSocket({ onCanvasDetail: onCanvasSync });
    const { listenCanvasErrorEvent, stopListenCanvasErrorEvent } = useCanvasErrorSocket({
      onCanvasError: (payload) => onError?.(payload),
    });

    /*
     * EDITOR PRESENCE — live as of #622.
     *
     * The transport is the project SSE plane, not a socket: the heartbeat is
     * POST .../{canvasId}/presence and the fan-out is the project event stream
     * this app already opens. `useCanvasPresence` owns the beat schedule and
     * the roster; this component owns what the roster MEANS on screen.
     *
     * `isReadOnly` is the reference's rule, unchanged: an empty roster is
     * editable by anyone, and once somebody real holds the canvas everyone
     * else is read-only. With no second editor, nothing here changes — which
     * is the acceptance criterion this must not break, and which is exactly
     * what a caller passing NO `viewer` broke: the roster answered to this
     * tab's own beat always holds this tab, so an editor with no identity
     * counted itself as the second editor and went read-only on a canvas
     * nobody else had open.
     *
     * Deliberately NOT a lock. The server refuses no write on this roster (see
     * internal/api/v2/canvaspresence's package doc), so the notice below still
     * tells the truth about last-write-wins for the person holding it.
     */
    const presence = useCanvasPresence({
      projectId,
      canvasId: selectedCodeBlockInfo?.canvasId,
      userName: viewer?.name,
      userId: viewer?.id,
      state: 'editing',
      enabled: !!selectedCodeBlockInfo?.canvasId && !viewOnly && !selectedCodeBlockInfo?.viewOnly,
    });

    /**
     * The read-only state the panes actually get. Two independent reasons, ORed
     * rather than merged into the `readOnly` state: the state is the CALLER's
     * intent (`viewOnly`, reset by its own effect) and presence is a live fact
     * from other people. Writing presence into that state would make the two
     * race, and the effect would clear it on the next prop change.
     */
    const effectiveReadOnly = readOnly || presence.isReadOnly;

    /*
     * Mermaid quick-fix. The runner and the four-condition capability gate live
     * in `../../model/useMermaidQuickFix.ts`; the caller injects the result as
     * `quickFix`. `mermaidError` is the RENDER error `MermaidDiagram` reports —
     * the control is only offered for a diagram that actually failed.
     */
    const [mermaidError, setMermaidError] = useState<string>('');

    const onQuickFixed = useCallback(
      (fixedCode: string) => {
        setCode(fixedCode);
        editorRef.current?.setCode(fixedCode);
        notifyChange(fixedCode);
      },
      [notifyChange],
    );

    // Header actions, dispatched to whichever pane is mounted
    // (baseline `CanvasEditor.jsx:248-266` for the table half).
    const onUndo = useCallback(() => activeEditor()?.undo(), [activeEditor]);
    const onRedo = useCallback(() => activeEditor()?.redo(), [activeEditor]);

    /*
     * FULL SCREEN. State per editor SESSION, deliberately: this component
     * unmounts when the drawer closes, so opening the next canvas starts at
     * the normal size rather than in whatever mode the previous document was
     * left in. There is nothing to persist — the mode belongs to the reading,
     * not to the canvas.
     *
     * It is rendered by re-anchoring THIS element to the viewport rather than
     * by widening the drawer that hosts it. The drawer is a portal with its
     * own paper width, and asking the composition root to change it would put
     * a mode this component owns into a component that does not know it
     * exists.
     */
    const [isFullScreen, setIsFullScreen] = useState(false);
    const onToggleFullScreen = useCallback(() => {
      setIsFullScreen((current) => !current);
    }, []);

    /**
     * The editor's own keyboard — the rule is `../../lib/canvasEditorKeys`,
     * this is the dispatch. See that module for why a press inside CodeMirror
     * is deliberately left alone and why Escape stops here in full screen.
     */
    const onKeyDown = useCallback(
      (event: React.KeyboardEvent<HTMLElement>) => {
        const fromCodePane = event.target instanceof Element && event.target.closest('.cm-editor') !== null;
        const action = canvasEditorKeyAction(event, { isFullScreen, fromCodePane });
        if (action === 'ignore') return;
        event.preventDefault();
        // The drawer this editor sits in closes on Escape, and closing it
        // SAVES. One press must not both collapse the editor and end the
        // session.
        event.stopPropagation();
        if (action === 'exit-full-screen') setIsFullScreen(false);
        else if (action === 'undo') activeEditor()?.undo();
        else activeEditor()?.redo();
      },
      [activeEditor, isFullScreen],
    );

    /** Saves the LIVE table — the same read Copy and Save make, for the same reason. */
    const onExportTable = useCallback(
      (format: TableExportFormat) => {
        void exportTable(parseMarkdownTable(tableRef.current?.getCode() ?? code), format).catch((cause: unknown) =>
          onError?.(cause),
        );
      },
      [code, onError],
    );
    const onClickAddColumn = useCallback(() => tableRef.current?.addColumn(), []);
    const onClickAddRow = useCallback(() => tableRef.current?.addRow(), []);
    const onDeleteSelectedRowsOrColumns = useCallback(() => tableRef.current?.delete(), []);

    // Title for the editor header
    const title = useMemo(
      () => {
        if (!selectedCodeBlockInfo?.isBlock) return 'Edit response';
        if (codeLanguage === 'markdownTable') return 'Edit table';
        if (codeLanguage === 'mermaid') return 'Edit diagram';
        if (codeLanguage === 'document') return 'Edit document';
        return 'Edit code';
      },
      [selectedCodeBlockInfo?.isBlock, codeLanguage],
    );

    // Handle language change
    const onChangeLanguage = useCallback(
      (newLanguage: string) => {
        setCodeLanguage(newLanguage);
        if (editCanvas && selectedCodeBlockInfo?.canvasId) {
          void editCanvas({
            projectId: projectId ?? 0,
            canvasUUID: selectedCodeBlockInfo.canvasId,
            code_language: newLanguage,
            canvas_type: 'code',
            name: title,
          });
        }
      },
      [editCanvas, projectId, selectedCodeBlockInfo?.canvasId, title],
    );

    // Imperative save handle
    useImperativeHandle(ref, () => ({
      save: () => onCloseEditor(),
    }));

    // Initialize code from selected code block
    useEffect(() => {
      if (selectedCodeBlockInfo?.codeBlock) {
        setCode(selectedCodeBlockInfo.codeBlock);
      }
    }, [selectedCodeBlockInfo?.codeBlock]);

    // Update read-only state when it changes
    useEffect(() => {
      setReadOnly(selectedCodeBlockInfo?.viewOnly ?? viewOnly);
    }, [selectedCodeBlockInfo?.viewOnly, viewOnly]);

    // Join the canvas socket room on mount (if canvasId is present)
    useEffect(() => {
      if (selectedCodeBlockInfo?.canvasId) {
        setTimeout(() => {
          listenCanvasDetailEvent();
          listenCanvasSyncEvent();
          listenCanvasErrorEvent();
        }, 0);
      }
      return () => {
        stopListenCanvasSyncEvent();
        stopListenCanvasDetailEvent();
        stopListenCanvasErrorEvent();
      };
    }, [selectedCodeBlockInfo?.canvasId, listenCanvasDetailEvent, listenCanvasSyncEvent, listenCanvasErrorEvent, stopListenCanvasSyncEvent, stopListenCanvasDetailEvent, stopListenCanvasErrorEvent]);

    // The canvas room is joined and LEFT by `useCanvasRoom` above (#622); the
    // pending remote edit is what this editor still owns. Dropping it on the way
    // out is deliberate: a debounced broadcast that fires after unmount would
    // push a document nobody is editing any more.
    useEffect(() => {
      return () => {
        if (remoteEmitTimerRef.current !== undefined) {
          clearTimeout(remoteEmitTimerRef.current);
          remoteEmitTimerRef.current = undefined;
        }
      };
    }, []);

    const onCloseEditor = useCallback(
      () => {
        // The LIVE document, for the same reason Copy reads it: `code` only
        // catches up after the editor's change debounce, so closing right
        // after the last keystroke would hand the caller the previous
        // revision to save.
        onCloseCanvasEditor(canUndo, activeEditor()?.getCode() ?? code, codeLanguage);
      },
      [activeEditor, canUndo, code, codeLanguage, onCloseCanvasEditor],
    );

    /*
     * The honest statement of what the editor does NOT do.
     *
     * A shared canvas (one with a `canvasId`) is reachable by everyone who
     * can see the conversation, and the presence/read-only lock the baseline
     * had is deliberately not wired here (deviation 6). So a save writes the
     * whole document and whichever save lands last is the one that survives.
     * Only a shared, editable canvas can lose someone's work this way, so the
     * notice is shown for exactly that case — a read-only view has nothing to
     * lose, and a canvas with no id has no second editor.
     *
     * It must not imply a lock, a merge or a live document, because there is
     * none of the three.
     */
    /*
     * WHO ELSE IS HERE. The reference renders the same thing — an avatar stack
     * and "is editing…" (apps/elitea-ui/src/components/Canvas.jsx:69-84) — with
     * up to three overlapping 20px avatars and a `+N` counter, initials on a
     * name-derived colour where there is no picture. `user_avatar` is absent
     * from this server's roster today, so every avatar is initials.
     *
     * Nothing renders when nobody else is on the canvas, which is why the
     * "unchanged with no second editor" acceptance criterion holds.
     */
    const presenceRow =
      presence.otherEditors.length > 0 ? (
        <Box sx={{ display: 'flex', alignItems: 'center', gap: '8px', padding: '0 4px' }} data-testid="canvas-presence">
          <Tooltip title={presence.otherEditors.map((editor) => editor.userName).join(', ')} placement="top">
            <Box sx={{ display: 'flex', alignItems: 'center' }}>
              {presence.otherEditors.slice(0, MAX_PRESENCE_AVATARS).map((editor, index) => (
                <Avatar
                  key={editor.userName}
                  alt={editor.userName}
                  data-testid="canvas-presence-avatar"
                  sx={{
                    width: '1.25rem',
                    height: '1.25rem',
                    fontSize: '0.625rem',
                    transform: `translateX(-${String(index * 5)}px)`,
                    zIndex: presence.otherEditors.length - index,
                    backgroundColor: stringToColor(editor.userName),
                  }}
                >
                  {getInitials(editor.userName)}
                </Avatar>
              ))}
              {presence.otherEditors.length > MAX_PRESENCE_AVATARS && (
                <Typography variant="bodySmall">{`+${String(presence.otherEditors.length - MAX_PRESENCE_AVATARS)}`}</Typography>
              )}
            </Box>
          </Tooltip>
          <Typography variant="bodySmall" color="text.primary">
            {presence.otherEditors.length === 1
              ? t('canvas.presence.oneEditing', '{{name}} is editing…', { name: presence.otherEditors[0]?.userName ?? '' })
              : // NOT `count`: i18next treats that option name as a plural selector and
                // looks for `_one`/`_other` sibling keys. `people` is a plain
                // interpolation variable, which is what this string actually needs.
                t('canvas.presence.manyEditing', '{{people}} people are editing…', { people: presence.otherEditors.length })}
          </Typography>
        </Box>
      ) : null;

    const concurrentEditNotice =
      selectedCodeBlockInfo?.canvasId && !effectiveReadOnly ? (
        <Typography variant="labelSmall" color="text.secondary" sx={{ padding: '0 4px' }}>
          {t(
            'canvas.editor.concurrentEdits',
            'Anyone with access can edit this canvas. Edits are not merged — the last save replaces earlier ones.',
          )}
        </Typography>
      ) : null;

    // Determine if we should show the editor at all
    if (!selectedCodeBlockInfo?.codeBlock && !selectedCodeBlockInfo?.isCreatingCanvas) {
      return <Box sx={{ display: 'none' }} />;
    }

    // Loading state (new canvas creation)
    if (selectedCodeBlockInfo?.isCreatingCanvas) {
      return (
        <Box
          sx={{
            display: 'flex',
            flexDirection: 'column',
            height: '100%',
            maxHeight: '100%',
            minWidth: '240px',
            gap: '8px',
          }}
        >
          <CanvasEditHeader
            title={title}
            actions={{
              onClose: onCloseEditor,
              disableUndo: true,
              disableRedo: true,
            }}
            langSelect={{ disableLanguageSelect: true }}
            disabledAll
          />
          <Box
            sx={{
              flex: 1,
              display: 'flex',
              justifyContent: 'center',
              alignItems: 'center',
              borderRadius: '8px',
              border: '1px solid',
              borderColor: 'divider',
              background: '#fafafa',
            }}
          >
            <Typography variant="labelMedium">Loading the canvas...</Typography>
          </Box>
        </Box>
      );
    }

    // Error state (canvas creation failed)
    if (selectedCodeBlockInfo?.createCanvasError) {
      return (
        <Box
          sx={{
            display: 'flex',
            flexDirection: 'column',
            height: '100%',
            maxHeight: '100%',
            minWidth: '240px',
            gap: '8px',
          }}
        >
          <CanvasEditHeader
            title={title}
            actions={{
              onClose: onCloseEditor,
              disableUndo: true,
              disableRedo: true,
            }}
            langSelect={{ disableLanguageSelect: true }}
            disabledAll
          />
          <Box
            sx={{
              flex: 1,
              display: 'flex',
              justifyContent: 'center',
              alignItems: 'center',
              borderRadius: '8px',
              padding: '0 20px',
              border: '1px solid',
              borderColor: 'divider',
              background: '#fafafa',
              boxSizing: 'border-box',
            }}
          >
            <Typography variant="labelMedium" color="error">
              {JSON.stringify(selectedCodeBlockInfo.createCanvasError)}
            </Typography>
          </Box>
        </Box>
      );
    }

    // Main editor
    return (
      <Box
        data-testid="canvas-editor-root"
        data-fullscreen={isFullScreen ? 'true' : 'false'}
        onKeyDown={onKeyDown}
        sx={(theme) => ({
          display: 'flex',
          flexDirection: 'column',
          height: '100%',
          maxHeight: '100%',
          minWidth: '240px',
          gap: '8px',
          /*
           * Full screen re-anchors this element to the VIEWPORT. `position:
           * fixed` escapes the drawer's own paper without the drawer being
           * told anything, and the ground has to be painted explicitly — the
           * drawer's paper is behind it, not under it, once this is fixed.
           * One step above the modal layer so the drawer does not overlap it.
           */
          ...(isFullScreen
            ? {
                position: 'fixed',
                inset: 0,
                zIndex: theme.zIndex.modal + 1,
                backgroundColor: theme.vars.palette.background.paper,
                padding: '1rem',
                boxSizing: 'border-box',
                maxHeight: '100vh',
              }
            : {}),
        })}
      >
        <CanvasEditHeader
          title={title}
          actions={{
            onClose: onCloseEditor,
            onUndo,
            disableUndo: !canUndo,
            onRedo,
            disableRedo: !canRedo,
            onCopy: () => {
              // Read the LIVE document of the ACTIVE pane, not the debounced
              // `code` mirror: a copy fired within the change debounce would
              // otherwise put the previous revision on the clipboard.
              const current = activeEditor()?.getCode() ?? code;
              navigator.clipboard.writeText(current).catch((cause: unknown) => onError?.(cause));
            },
            onRegenerate,
            onDelete,
            onToggleFullScreen,
            isFullScreen,
            ...(saveToArtifacts ? { onSaveToArtifacts: () => setSaveDialogOpen(true) } : {}),
          }}
          langSelect={{
            // `document` is not a CodeMirror language — like `markdownTable`,
            // it is its own top-level canvas type (selection affordance,
            // file open, or the answer's "Open as document" action), never
            // reached by picking it out of this dropdown.
            showLangSelect: codeLanguage !== 'markdownTable' && codeLanguage !== 'document',
            onChangeLanguage: onChangeLanguage,
            language: codeLanguage,
            disableLanguageSelect: codeLanguage === 'mermaid',
          }}
          isThisWholeMessage={!selectedCodeBlockInfo?.isBlock}
          table={{
            isTableEditing,
            hasSelectedRowsColumns,
            onClickAddColumn,
            onClickAddRow,
            onDeleteSelectedRowsOrColumns,
            onImportTableData,
            onImportError: onError,
            onExportTable,
          }}
          disabledAll={effectiveReadOnly || selectedCodeBlockInfo?.isCreatingCanvas || !!selectedCodeBlockInfo?.createCanvasError}
        />
        {saveToArtifacts && (
          <SaveToArtifactsDialog
            open={saveDialogOpen}
            projectId={projectId !== undefined ? String(projectId) : ''}
            // The LIVE document — the same read Copy and Save-close make, for
            // the same reason: `code` only catches up after the editor's
            // change debounce.
            content={activeEditor()?.getCode() ?? code}
            {...(saveToArtifacts.source !== undefined ? { source: saveToArtifacts.source } : {})}
            {...(saveToArtifacts.suggestedName !== undefined ? { suggestedName: saveToArtifacts.suggestedName } : {})}
            onClose={() => setSaveDialogOpen(false)}
            onSaved={(source) => {
              setSaveDialogOpen(false);
              saveToArtifacts.onSaved(source);
            }}
          />
        )}
        {presenceRow}
        {concurrentEditNotice}
        {codeLanguage === 'mermaid' ? (
          /* Mermaid split-view (baseline uses react-split; simplified to flex here) */
          <Box
            sx={{
              flex: 1,
              display: 'flex',
              flexDirection: 'column',
              gap: '8px',
              minWidth: '100%',
              width: '100%',
            }}
          >
            <Box
              sx={{
                overflow: 'scroll',
                minWidth: '100%',
                width: '100%',
                flex: 1,
                borderRadius: '8px',
                border: '1px solid',
                borderColor: 'divider',
                background: '#fafafa',
                boxSizing: 'border-box',
              }}
            >
              <CodeMirrorEditor
                ref={editorRef}
                value={code}
                onChange={notifyChange}
                history={historyCallbacks}
                readOnly={effectiveReadOnly}
                extensions={codeExtensions}
                height="100%"
                minHeight="240px"
                aria-label={title}
              />
            </Box>
            <Box
              sx={{
                minHeight: '500px',
                border: '1px solid',
                borderColor: 'divider',
                borderRadius: '8px',
                background: '#fafafa',
                boxSizing: 'border-box',
              }}
            >
              {/*
                Baseline: `<MermaidDiagramOutput code={code} onQuickFix={readOnly ? undefined : handleQuickFix} />`.
                `shared/ui/MermaidDiagram` is the RENDER half; the quick-fix half
                (the model round trip that rewrites broken diagram source) is the
                sibling control below, which reads the render error this reports.
              */}
              <MermaidDiagram
                code={code}
                onError={setMermaidError}
                data-testid="canvas-mermaid-diagram"
              />
              {quickFix && mermaidError !== '' && (
                <MermaidQuickFixButton
                  quickFix={quickFix}
                  error={mermaidError}
                  code={code}
                  onFixed={onQuickFixed}
                  onError={onError}
                />
              )}
            </Box>
          </Box>
        ) : codeLanguage === 'markdownTable' ? (
          /* Markdown table editor */
          <Box
            sx={{
              overflow: 'scroll',
              minWidth: '100%',
              width: '100%',
              flex: 1,
              borderRadius: '8px',
              border: '1px solid',
              borderColor: 'divider',
              background: '#fafafa',
              boxSizing: 'border-box',
            }}
          >
            <MarkdownTableEditor
              ref={tableRef}
              content={{ initialMarkdown: code, onChange: notifyChange }}
              history={tableHistoryCallbacks}
              onRowsColumnsSelected={setHasSelectedRowsColumns}
              readOnly={effectiveReadOnly}
              tracking={{ interaction_uuid, conversation_uuid }}
            />
          </Box>
        ) : codeLanguage === 'document' ? (
          /* Document (rich-text) editor — issue #879. Lazy-loaded: see `./DocumentEditor.tsx`'s module doc. */
          <Box
            sx={{
              overflow: 'hidden',
              minWidth: '100%',
              width: '100%',
              flex: 1,
              borderRadius: '8px',
              border: '1px solid',
              borderColor: 'divider',
              background: '#fafafa',
              boxSizing: 'border-box',
            }}
          >
            <Suspense
              fallback={
                <Box sx={{ display: 'flex', justifyContent: 'center', alignItems: 'center', height: '100%' }}>
                  <Typography variant="labelMedium">{t('features.chatMessages.canvas.document.loading', 'Loading the document editor…')}</Typography>
                </Box>
              }
            >
              <LazyDocumentEditor
                ref={docRef}
                content={{ initialMarkdown: code, onChange: notifyChange }}
                history={docHistoryCallbacks}
                readOnly={effectiveReadOnly}
                aria-label={title}
              />
            </Suspense>
          </Box>
        ) : (
          /* Code editor */
          <Box
            sx={{
              overflow: 'scroll',
              minWidth: '100%',
              width: '100%',
              flex: 1,
              borderRadius: '8px',
              border: '1px solid',
              borderColor: 'divider',
              background: '#fafafa',
              boxSizing: 'border-box',
            }}
          >
            <CodeMirrorEditor
              ref={editorRef}
              value={code}
              onChange={notifyChange}
              history={historyCallbacks}
              readOnly={effectiveReadOnly}
              extensions={codeExtensions}
              height="100%"
              minHeight="240px"
              aria-label={title}
            />
          </Box>
        )}
      </Box>
    );
  },
);

CanvasEditor.displayName = 'CanvasEditor';
