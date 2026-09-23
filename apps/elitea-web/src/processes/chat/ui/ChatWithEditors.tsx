import type { ReactNode } from 'react';
import { useCallback } from 'react';

import { useParams, useRouteContext } from '@tanstack/react-router';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';

import ChatPage from '@/pages/chat';
import { t } from '@/shared/i18n';
import { DeleteEntityModal } from '@/shared/ui/DeleteEntityModal';
import { useSelectedProject } from '@/widgets/app-shell';

import { ChatConversationSidebar } from './ChatConversationSidebar';
import { ChatPlayback } from './ChatPlayback';
import { usePlaybackConversationId } from './usePlaybackConversationId';
import { usePlusMenuEntities } from '../model/usePlusMenuEntities';
import { useChatWithEditors } from './ChatWithEditors.hooks';
import { ChatCanvasDrawer } from './ChatCanvasDrawer';
import { ChatEditorColumn } from './ChatEditorColumn';
import { useCanvasCreation } from './useCanvasCreation';
import { useChatPipelineConfig } from './useChatPipelineConfig';
import { useCreateChatReset } from './useCreateChatReset';
import { FileCanvasDrawer } from './FileCanvasDrawer';
import { useFileCanvas } from './useFileCanvas';

/**
 * Who is signed in, read off the router root context's `auth` seam — the same
 * local-copy shape every other reader of that seam uses
 * (`pages/user-public/api/useRouterAuth.ts` states why it is copied rather
 * than imported across a slice boundary).
 *
 * It is read HERE and not inside `useCanvasEditing`, because this component is
 * always mounted under `<RouterProvider>` and that hook is not: reading the
 * route context from a hook a plain `renderHook` can mount would make the hook
 * throw outside a router.
 *
 * The id is the one the canvas presence roster is keyed by:
 * `/forward-auth/info` answers `user_id` and `/social/author` answers `id`,
 * and both are the principal id the presence handler writes as
 * `Editor.user_id`.
 */
interface ViewerRouterContext {
  readonly auth?: { readonly getUser?: () => { readonly id?: string } | undefined };
}

/**
 * Pure extraction, kept module-local: both of its branches are covered through
 * the real route by `chatCanvasOpener.test.tsx` (a viewer that is on the roster
 * keeps editing; a roster held by somebody else goes read-only), which is where
 * the wiring itself can be observed at all.
 */
function selectViewerId(context: unknown): string | undefined {
  if (typeof context !== 'object' || context === null) return undefined;
  const id = (context as ViewerRouterContext).auth?.getUser?.()?.id;
  return id === undefined || id === '' ? undefined : id;
}

/**
 * `ChatWithEditors` — the real composition root this whole unit exists to
 * build. `src/features/pipelines/ui/PipelineEditor.tsx`'s own module doc
 * comment (its "STILL UNREACHABLE from the live app" section) already
 * diagnosed the exact gap this file closes: `AgentEditor`/`PipelineEditor`/
 * `ToolkitEditor` are fully built and tested but nothing in the live app
 * mounts them, because `processes/chat/model/useEditorMutex.ts` (the
 * intended orchestrator) was itself never called anywhere, and
 * `widgets/chat-box/ui/ChatBox.helpers.ts`'s `buildAgentEditorProps` wired
 * `onShowAgentEditor`/`onShowPipelineEditor`/`onCloseAgentEditor`/
 * `onClosePipelineEditor` as literal no-ops.
 *
 * **Why this lives in `processes/chat/ui/`, not `widgets/chat-box/` —
 * architectural correction, not a stylistic choice.** `.dependency-
 * cruiser.cjs`'s `LAYERS_ABOVE` table gives `processes: ['app']`: only
 * `src/app/` may import from `src/processes/`, but `processes/` itself may
 * legally import `widgets/`, `pages/`, `features/`, `entities/`, `shared/`
 * — everything BELOW it. A composition root that needs `widgets/chat-box`
 * (via `pages/chat`), `features/agents`, `features/pipelines`,
 * `features/toolkits`, AND `processes/chat/model/useEditorMutex` all at
 * once therefore belongs here, one layer above every one of them — mounting
 * it from `widgets/chat-box/ui/ChatBox.tsx` directly would be the exact
 * upward import `no-upward-from-widgets` forbids. `src/routes/_shell/
 * chat.tsx` (unconstrained by any layer regex — verified against
 * `.dependency-cruiser.cjs`'s four `from` patterns, none of which match
 * `^src/routes/`) renders THIS component instead of `pages/chat`'s
 * `ChatPage` directly; `ChatPage` itself is still rendered here, as this
 * component's main child, extended with one new optional `editorCallbacks`
 * prop (`pages/chat/index.tsx`'s own doc comment) that reaches all the way
 * down to `features/chat-input`'s `NewChatInput` through `ChatBox`'s
 * matching new optional prop.
 *
 * All the real hook wiring (`useEditAgent`/`useAgentCreation`/
 * `useEditPipeline`/`usePipelineCreation`/`useEditToolkit`/
 * `useToolkitCreation`/the real toolkit create/save mutations
 * (`toolkitWriteDeps`)/`useEditorMutex`, plus every disclosed gap —
 * Canvas/Artifact stubs, the agent-participant-resync gap) lives in
 * `./ChatWithEditors.hooks.ts`, split
 * out purely to keep this file itself under the §3.5 400-line budget; read
 * that file's own doc comments for the full disclosure. This file is pure
 * composition: render `<ChatPage>` plus the three editors, each gated on
 * its own `shared/lib/editorState` flag, plus the two confirm dialogs
 * (`EditorShell`'s own discard-confirm is a separate, already-built
 * concern — the dialog rendered directly here is `useEditorMutex`'s own
 * "another editor is open" queue-confirm).
 */
export function ChatWithEditors(): ReactNode {
  // `strict: false`, this file's established convention (see
  // `usePlaybackConversationId.ts`'s own doc comment) for a component that
  // must not depend on which route file mounts it — `ChatWithEditors` is
  // mounted at `/_shell/chat`, which carries no `conversationId` itself;
  // the param only exists on the child route `/_shell/chat/$conversationId`.
  // Needed here (issue #867) so a created agent/pipeline/toolkit can be
  // attached to the conversation actually open right now.
  const { conversationId } = useParams({ strict: false }) as { conversationId?: string };
  const { project } = useSelectedProject();
  const projectId = project?.id === undefined ? undefined : String(project.id);

  const {
    isEditingAgent,
    isEditingPipeline,
    isEditingToolkit,
    agentForEditor,
    editAgent,
    agentCreation,
    editPipeline,
    pipelineCreation,
    editToolkit,
    toolkitCreation,
    toolkitWriteDeps,
    mutex,
    handleShowAgentEditor,
    handleShowPipelineEditor,
    handleShowToolkitEditor,
    handleShowCanvasEditor,
    canvas,
  } = useChatWithEditors({ projectId, conversationId });

  /*
   * The canvas CREATE. Called HERE and not inside `useChatWithEditors`, for
   * the same reason `viewerId` is read here: it needs the route — the
   * conversation the selection belongs to — and `useChatWithEditors` is
   * mounted by a plain `renderHook` in its own tests, where `useParams` has no
   * router to read and throws.
   *
   * It is deliberately NOT routed through the editor mutex either: making a
   * canvas opens no editor, so there is no second editor to queue behind. It
   * writes the canvas and re-reads the transcript; the reader then opens the
   * new block with the control every stored canvas already has.
   */
  const canvasCreation = useCanvasCreation();

  /*
   * "Open in canvas" from a message ATTACHMENT (issue #878). Composed HERE
   * for the same reason `canvasCreation` is: it needs `projectId`, and this
   * is the layer that has it without re-deriving it inside a hook a plain
   * `renderHook` mounts (`useFileCanvas`'s own tests do exactly that).
   */
  const fileCanvas = useFileCanvas(projectId);

  /*
   * `strict: false` reads the ROOT's merged context from any component under
   * `<RouterProvider>`, the same call `useRouterAuth.ts` makes.
   */
  const routeContext: unknown = useRouteContext({ strict: false });
  const viewerId = selectViewerId(routeContext);

  // "+ Create -> Chat" writes `?create=1` and had no reader, so the click did
  // nothing while the user was already on `/chat`. `resetToken` keys the chat
  // subtree, so each click remounts it; see `useCreateChatReset` for the full
  // disclosure. The editors close first, as the reference app did on the same
  // trigger.
  const closeEditors = useCallback(() => {
    editAgent.onCloseAgentEditor();
    editPipeline.onClosePipelineEditor();
    editToolkit.onCloseToolkitEditor();
  }, [editAgent, editPipeline, editToolkit]);
  const resetToken = useCreateChatReset(closeEditors);

  // The composer "+" menu's agent/pipeline/toolkit/MCP lists. This is the
  // lowest layer allowed to fetch them (`widgets/` may not import
  // `processes/`), so they are resolved here and handed down as a prop.
  const plusMenu = usePlusMenuEntities();

  // `?playback=1` on `/chat/$conversationId` replaces the live chat column
  // with the replay surface. See `ChatPlayback.tsx` for why the signal
  // travels through the URL rather than shared component state.
  const playbackConversationId = usePlaybackConversationId();

  /*
   * Whether the editor column exists at all. The canvas editors are NOT part
   * of it — both are MUI `Drawer`s, which portal out of this tree entirely,
   * so folding them in here would reserve a column for something that never
   * renders in it.
   */
  const isAnyEditorOpen = isEditingAgent || isEditingPipeline || isEditingToolkit;

  /*
   * The pipeline editor's EDIT-mode Configuration tab and its Save
   * (#940 A12, ELITEA-0928). Both deps come from one hook because they share
   * one draft — see `useChatPipelineConfig`'s own module doc comment.
   */
  const pipelineConfig = useChatPipelineConfig({
    projectId,
    participant: editPipeline.editingPipeline,
    enabled: isEditingPipeline && !editPipeline.isPipelineCreateMode,
  });

  return (
    <>
      {/*
        * Two columns: the conversation/folder rail and the chat itself.
        * `ChatConversationSidebar` is the mount `features/chat-conversation-
        * list` never had (issue #128 residual) — see that file's own module
        * doc for why the mount belongs at this layer. Baseline shape:
        * `NewChat.jsx:1360-1412` renders `<Conversations>` in the left Grid
        * column of the same row as the conversation pane.
        *
        * The row padding is that layout's own container padding
        * (`NewChat.jsx:1736-1743`, desktop `1rem 0rem 1rem 1.5rem`): the
        * chat surface is inset from the nav rail and the window edges, not
        * flush against them. There is deliberately no right padding — the
        * conversation column supplies the gutter on its side, and the chat
        * column runs to the edge so the participants rail can dock there.
        *
        * A THIRD column appears while an entity editor is open (#940 A12).
        * Before that, `<AgentEditor>`/`<PipelineEditor>`/`<ToolkitEditor>`
        * were siblings of this row inside the same fragment, so each one
        * rendered its own `height: 100%` block BELOW the chat inside a
        * `main` that is `display: block; height: 100vh` with no overflow of
        * its own (`widgets/app-shell/ui/AppShell.tsx`). The consequences are
        * exactly the four cases this change answers: the editor's Save /
        * Discard / Close header sat a viewport below the fold
        * (ELITEA-0922), the shell's own `overflow: auto` content area never
        * became the scroll container so the whole document scrolled instead
        * and ran straight past the form into whatever followed it
        * (ELITEA-0918, ELITEA-0928), and a form that grew — four
        * conversation starters — pushed the page further rather than
        * scrolling inside its own panel (ELITEA-0919, ELITEA-0920).
        *
        * As a bounded flex column it is `EditorShell`'s own layout that
        * works: a fixed header carrying Close/Discard/Save, and one
        * `overflow: auto` body under it. Nothing in `EditorShell` changed —
        * it was always written for this, it was simply never given a parent
        * with a height.
        */}
      <Box sx={{ display: 'flex', height: '100%', minHeight: 0, width: '100%', boxSizing: 'border-box', py: 2, pl: 3 }}>
        <ChatConversationSidebar />
        <Box
          sx={{
            flexGrow: 1,
            minWidth: 0,
            height: '100%',
            /*
             * Below `md` there is not room for both, and the editor is what
             * the user just asked for — so the chat column yields rather than
             * squeezing the form into an unusable width. It is hidden, not
             * unmounted: unmounting `ChatPage` would drop the composer's
             * draft and the transcript's scroll position every time an
             * editor opened.
             */
            ...(isAnyEditorOpen ? { display: { xs: 'none', md: 'block' } } : {}),
          }}
        >
          {playbackConversationId !== undefined ? (
            <ChatPlayback conversationId={playbackConversationId} />
          ) : (
          <ChatPage
            key={`chat-${String(resetToken)}`}
            entitySubmenus={{ ...plusMenu.entities, onOpen: plusMenu.onOpen }}
            editorCallbacks={{
              onShowAgentEditor: handleShowAgentEditor,
              onShowPipelineEditor: handleShowPipelineEditor,
              onShowToolkitEditor: handleShowToolkitEditor,
              onCloseAgentEditor: editAgent.onCloseAgentEditor,
              onClosePipelineEditor: editPipeline.onClosePipelineEditor,
              /*
                * "Create new" in the composer's "+" menu (issue #867).
                * `mutex.onCreate*` already exist — `useEditorMutex` was
                * built to serve exactly this, queuing behind an open editor
                * the same way `onEditAgent`/`onEditToolkit`/`onEditPipeline`
                * do — they were simply never read by anything until now.
                */
              onCreateAgent: mutex.onCreateAgent,
              onCreatePipeline: mutex.onCreatePipeline,
              onCreateToolkit: mutex.onCreateToolkit,
              /*
                * The transcript's canvas opener (issue 853). Both halves travel
                * together: the handler that opens a stored canvas block, and the
                * block currently open — the transcript swaps that one for an
                * editing placeholder so the same document is not shown twice.
                */
              onShowCanvasEditor: handleShowCanvasEditor,
              selectedCanvasBlock: canvas.selectedCodeBlockInfo,
              /* And the gesture that MAKES one: a range highlighted in an answer. */
              onCreateCanvasFromSelection: canvasCreation.onCreateCanvasFromSelection,
              /* A stored FILE, opened from a message attachment (issue #878). */
              onOpenFileInCanvas: fileCanvas.onOpenFileInCanvas,
            }}
          />
          )}
        </Box>
        {/*
          * The editor column. Rendered INSIDE the row (not as a sibling of
          * it) so it inherits the row's height; see the row's own comment
          * above for what the sibling placement cost. Its BODY lives in
          * `./ChatEditorColumn.tsx` — this file is at its §3.5 400-line
          * budget and the composition root's own branch count is at the
          * complexity budget, both of which the three editors' props push
          * over on their own.
          */}
        {isAnyEditorOpen && (
          <ChatEditorColumn
            agent={{ isEditing: isEditingAgent, agentForEditor, editAgent, agentCreation }}
            pipeline={{ isEditing: isEditingPipeline, editPipeline, pipelineCreation, config: pipelineConfig }}
            toolkit={{ isEditing: isEditingToolkit, editToolkit, toolkitCreation, toolkitWriteDeps }}
          />
        )}
      </Box>

      {/*
        * The canvas editor's mount, in its own file (`./ChatCanvasDrawer.tsx`)
        * for this file's §3.5 400-line and complexity budgets — see that
        * file's own doc comment for every prop it carries and why.
        */}
      <ChatCanvasDrawer canvas={canvas} viewerId={viewerId} />

      {/* The FILE canvas (issue #878) — a message attachment opened for editing. See `FileCanvasDrawer`'s own doc for why it is a second, independent drawer. */}
      <FileCanvasDrawer fileCanvas={fileCanvas} projectId={projectId} viewerId={viewerId} />

      {/* `useEditorMutex`'s own "another editor is open" queue-and-confirm flow (its own doc comment) — distinct from `EditorShell`'s own discard-confirm, which guards a single editor's own close/discard action. */}
      <DeleteEntityModal
        open={mutex.openEditingAlert}
        onClose={mutex.onCloseEditorAlert}
        onConfirm={mutex.onConfirmCloseEditor}
        copy={{
          title: t('processes.chat.chatWithEditors.queueConfirmTitle', 'Another editor is open'),
          confirmText: t('processes.chat.chatWithEditors.queueConfirmConfirm', 'Discard & Continue'),
          cancelText: t('processes.chat.chatWithEditors.queueConfirmCancel', 'Cancel'),
        }}
        content={{
          custom: (
            <Typography variant="bodyMedium">
              {t(
                'processes.chat.chatWithEditors.queueConfirmContent',
                'Discard the changes in the current editor and open the one you selected instead?',
              )}
            </Typography>
          ),
        }}
        data-testid="chat-editor-mutex-confirm"
      />
    </>
  );
}
