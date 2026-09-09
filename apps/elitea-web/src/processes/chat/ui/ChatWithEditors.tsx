import type { ReactNode } from 'react';
import { useCallback } from 'react';

import { useParams, useRouteContext } from '@tanstack/react-router';

import Box from '@mui/material/Box';
import Drawer from '@mui/material/Drawer';
import Typography from '@mui/material/Typography';

import { AgentEditor } from '@/features/agents';
import { CanvasEditor } from '@/features/chat-messages';
import { PipelineEditor } from '@/features/pipelines';
import { ToolkitEditor } from '@/features/toolkits';
import ChatPage from '@/pages/chat';
import { t } from '@/shared/i18n';
import { DeleteEntityModal } from '@/shared/ui/DeleteEntityModal';
import { useSelectedProject } from '@/widgets/app-shell';

import { ChatConversationSidebar } from './ChatConversationSidebar';
import { ChatPlayback } from './ChatPlayback';
import { usePlaybackConversationId } from './usePlaybackConversationId';
import { toCreatedResult } from './ChatWithEditors.helpers';
import { usePlusMenuEntities } from '../model/usePlusMenuEntities';
import { useChatWithEditors } from './ChatWithEditors.hooks';
import { renderAgentEditorShell, renderPipelineEditorShell, renderToolkitEditorShell } from './EditorShell';
import { useCanvasCreation } from './useCanvasCreation';
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
        */}
      <Box sx={{ display: 'flex', height: '100%', minHeight: 0, width: '100%', boxSizing: 'border-box', py: 2, pl: 3 }}>
        <ChatConversationSidebar />
        <Box sx={{ flexGrow: 1, minWidth: 0, height: '100%' }}>
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
      </Box>

      {isEditingAgent && (
        <AgentEditor
          agent={agentForEditor}
          isVisible={isEditingAgent}
          isCreateMode={editAgent.isCreateMode}
          onCloseAgentEditor={editAgent.onCloseAgentEditor}
          onAgentCreated={(result) => void agentCreation.onAgentCreated(toCreatedResult(result))}
          deps={{ renderShell: renderAgentEditorShell }}
        />
      )}

      {isEditingPipeline && (
        <PipelineEditor
          pipeline={editPipeline.editingPipeline}
          isVisible={isEditingPipeline}
          isCreateMode={editPipeline.isPipelineCreateMode}
          onClosePipelineEditor={editPipeline.onClosePipelineEditor}
          onPipelineCreated={(result) => pipelineCreation.onPipelineCreated(toCreatedResult(result))}
          deps={{ renderShell: renderPipelineEditorShell }}
        />
      )}

      {/*
        * The canvas editor's mount — `CanvasEditor.tsx` was fully built and
        * rendered by nothing, which is also what made `useEditorMutex`'s
        * save-before-swap branch a no-op (see `useCanvasEditing`).
        *
        * Rendered as a right-hand drawer rather than the baseline's
        * resizable split pane: `react-split` is not a dependency here, and
        * the port's own module doc already states that trade.
        */}
      {canvas.isEditingCanvas && canvas.selectedCodeBlockInfo !== undefined && (
        <Drawer
          anchor="right"
          open
          /*
           * `() => …`, not the handler itself. MUI calls `onClose(event,
           * reason)`, and `onCloseCanvasEditor`'s own contract is
           * `(hasChange, finalResult, language)` — handed the pair directly it
           * would read the event as "there are changes" and the string
           * `"backdropClick"` as the document to save, and write that over the
           * user's canvas.
           */
          onClose={() => canvas.onCloseCanvasEditor()}
          slotProps={{ paper: { sx: { width: { xs: '100%', md: '48rem' }, maxWidth: '100%', p: 2, boxSizing: 'border-box' } } }}
          data-testid="chat-canvas-editor"
        >
          <CanvasEditor
            ref={canvas.canvasEditorRef}
            selectedCodeBlockInfo={canvas.selectedCodeBlockInfo}
            onCloseCanvasEditor={canvas.onCloseCanvasEditor}
            /*
             * The language picker's own write. It was omitted, so
             * `CanvasEditor`'s `onChangeLanguage` guard (`if (editCanvas &&
             * …)`) was permanently false and switching a canvas's language
             * changed the highlighting and nothing else.
             */
            editCanvas={canvas.editCanvas}
            {...(canvas.projectId !== undefined ? { projectId: canvas.projectId } : {})}
            /*
             * Who is looking. It was omitted, and that is not a cosmetic gap:
             * the presence roster the editor reads back from its OWN first
             * beat contains this tab, so an editor with no viewer identity
             * counted itself as "somebody else is editing this canvas" and
             * mounted read-only — CodeMirror with `aria-readonly`, the table
             * grid with every cell disabled. A single user could not type in
             * their own canvas.
             */
            {...(viewerId !== undefined ? { viewer: { id: viewerId } } : {})}
            /*
             * "Save to artifacts" (issue #878) — offered here too, not only
             * on a file-opened canvas: a canvas MADE from a turn has no
             * artifact identity yet, so every save here is a "save as" (no
             * `source` to pre-fill), and — deliberately NOT threaded through
             * `canvas`'s own save (`editCanvas`, the `elitea_core` PUT) — the
             * two are independent actions: this one additionally publishes
             * the document as a browsable file, it does not replace the
             * canvas's own storage.
             */
            {...(canvas.projectId !== undefined ? { saveToArtifacts: { onSaved: () => undefined } } : {})}
          />
        </Drawer>
      )}

      {/* The FILE canvas (issue #878) — a message attachment opened for editing. See `FileCanvasDrawer`'s own doc for why it is a second, independent drawer. */}
      <FileCanvasDrawer fileCanvas={fileCanvas} projectId={projectId} viewerId={viewerId} />

      {isEditingToolkit && (
        <ToolkitEditor
          toolkit={editToolkit.editingToolkit}
          isVisible={isEditingToolkit}
          onCloseToolkitEditor={editToolkit.onCloseToolkitEditor}
          onToolkitCreated={(result) => void toolkitCreation.onToolkitCreated(result)}
          deps={{
            renderShell: renderToolkitEditorShell,
            createToolkit: toolkitWriteDeps.createToolkit,
            saveToolkit: toolkitWriteDeps.saveToolkit,
          }}
        />
      )}

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
