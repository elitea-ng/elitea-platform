import { useCallback, useMemo } from 'react';

import { agentEditorHooks } from '@/features/agents';
import { useEditPipeline, usePipelineCreation } from '@/features/pipelines';
import { toolkitEditorHooks, useToolkitCreate, useToolkitEdit } from '@/features/toolkits';
import type { CanvasEditPayload } from '@/features/chat-messages';
import type { Participant } from '@/entities/participant';
import { ChatParticipantType } from '@/shared/lib/chat';
import { useEditorStateStore } from '@/shared/lib/editorState';
import { t } from '@/shared/i18n';
import { useNavBlockerStore } from '@/widgets/app-shell';

import {
  readAgentParticipantSnapshot,
  readPipelineParticipantSnapshot,
  readToolkitParticipantSnapshot,
  toAgentParticipantSnapshot,
  toPipelineParticipantSnapshot,
  toToolkitParticipantSnapshot,
} from '../lib/editorParticipantAdapters';
import { useAttachCreatedParticipant, toAddedParticipantLike, type AttachCreatedParticipant } from '../lib/useAttachCreatedParticipant';
import { useEditorMutex, type EditorOpenInfo } from '../model/useEditorMutex';
import { useCanvasEditing } from './useCanvasEditing';

/**
 * The real wiring behind `ChatWithEditors.tsx` — split into this sibling
 * file purely to keep `ChatWithEditors.tsx` itself under the §3.5 400-line
 * budget (the composition root's own JSX is already substantial once three
 * editors + two confirm dialogs are laid out). Everything here is plain
 * hook composition, no JSX.
 */

const NAV_BLOCK_WARNING = t(
  'processes.chat.chatWithEditors.navBlockWarning',
  'You have unsaved changes in the editor. Are you sure you want to leave?',
);


/**
 * **DISCLOSED GAP — `activeParticipant`/`setActiveParticipant`/
 * `onChangeParticipantSettings` (`useEditAgent`'s optional participant-sync
 * params) are not wired.** Both are about keeping the currently-EDITED
 * agent's local state in sync with the conversation's ACTIVE participant
 * (the resync effect, `useEditAgent.ts`'s own `useEffect`) and about
 * writing a saved agent's data back into it (`handleAgentSaved`). That state
 * — `activeParticipant`/`setActiveParticipant` — is owned INSIDE
 * `pages/chat/index.tsx`'s `ChatPage` (local `useState`, never exposed
 * upward) and never reaches this composition root, which only renders
 * `<ChatPage>` as an opaque child (per this unit's own design — `ChatPage`
 * gains new OPTIONAL callback props, not a lifted-up participant value).
 * All three params are OPTIONAL on `UseEditAgentParams` precisely because
 * the baseline hook already tolerates their absence (`handleAgentSaved`'s
 * own first line: `if (!savedData || !activeParticipant ||
 * !setActiveParticipant) return;`), so omitting them here is a safe,
 * disclosed degradation — not a silent invention — matching the "disclosed
 * gap, not silently invented" convention `PipelineEditor.tsx`'s own module
 * doc comment establishes. Consequence: editing an agent that is ALSO the
 * conversation's current active participant will not auto-resync if that
 * participant changes elsewhere while the editor is open, and a completed
 * SAVE (not CREATE — creation already routes through `onAgentCreated`
 * below) will not push its refreshed variables/version id back onto the
 * active participant. Real fix requires lifting `ChatPage`'s
 * `activeParticipant` state (or an equivalent controlled-value bridge) up
 * to this composition root — a `pages/chat` contract change bigger than
 * this unit's "wire the already-built editors" scope.
 */
function useAgentEditing(isEditingAgent: boolean, attach: AttachCreatedParticipant) {
  const setAgentEditingBlockNav = useCallback((blocked: boolean) => {
    useEditorStateStore.getState().setEditingAgent(blocked);
    useNavBlockerStore.getState().setBlockNav(blocked, NAV_BLOCK_WARNING);
  }, []);

  const editAgent = agentEditorHooks.useEditAgent({ navBlocker: { isEditingAgent, setAgentEditingBlockNav } });

  /**
   * **`addNewParticipants` is real now (issue #867)** — POSTs the
   * freshly-created agent onto the current conversation via `attach`
   * (`../lib/useAttachCreatedParticipant.ts`). **`onSetActiveParticipant`
   * stays a disclosed no-op**: activating a participant is `ChatPage`'s own
   * local `useState`, not exposed upward here (same gap
   * `activeParticipant`/`setActiveParticipant` already discloses above) —
   * a created agent is attached, not auto-selected as active.
   */
  const agentCreation = agentEditorHooks.useAgentCreation({
    // `useAgentCreation`'s own `CreatedAgentParticipant.entity_settings.variables`
    // is `readonly unknown[]` (element shape unvalidated) — narrower than
    // `useEditAgent`'s own `EditAgentParticipant.entity_settings.variables?:
    // readonly AgentVariable[]`, so `editAgent.onAgentEditorCreated` cannot be
    // passed straight through (`unknown[]` is not assignable to a
    // `{name?,value?}[]`). This wrapper forwards only the two fields both
    // sides actually declare (`entity_meta.id`/`entity_settings.version_id`) —
    // `variables` is intentionally not carried over; the freshly-created
    // agent has no participant-scoped variable VALUES yet regardless.
    onAgentEditorCreated: (createdAgent) => {
      editAgent.onAgentEditorCreated({
        ...(createdAgent.entity_meta?.id !== undefined ? { entity_meta: { id: createdAgent.entity_meta.id } } : {}),
        ...(createdAgent.entity_settings?.version_id !== undefined
          ? { entity_settings: { version_id: createdAgent.entity_settings.version_id } }
          : {}),
      });
    },
    addNewParticipants: async (participants, onAdded) => {
      const created = participants[0];
      if (!created) return;
      const added = await attach({
        entity_name: ChatParticipantType.Applications,
        entity_meta: { id: created.id },
        ...(created.version_details?.id !== undefined ? { entity_settings: { version_id: created.version_details.id } } : {}),
      });
      if (added) onAdded(added.map(toAddedParticipantLike));
    },
    onSetActiveParticipant: () => {},
  });

  return { editAgent, agentCreation };
}

function usePipelineEditing(isEditingPipeline: boolean, attach: AttachCreatedParticipant) {
  const setPipelineEditingBlockNav = useCallback((blocked: boolean) => {
    useEditorStateStore.getState().setEditingPipeline(blocked);
    useNavBlockerStore.getState().setBlockNav(blocked, NAV_BLOCK_WARNING);
  }, []);

  const editPipeline = useEditPipeline({ navBlocker: { isEditingPipeline, setPipelineEditingBlockNav } });
  // `addNewParticipants` is real now (issue #867): `usePipelineCreation`
  // already builds a fully wire-shaped `CreatedPipelineParticipant`
  // (`entity_name`/`entity_meta`/`entity_settings` all present), so this
  // just posts it through `attach` — no reshaping needed, unlike the agent/
  // toolkit wrappers above. `onSetActiveParticipant` stays omitted: same
  // disclosed "ChatPage owns the active participant, not reachable from
  // here" gap `useAgentEditing` states.
  const pipelineCreation = usePipelineCreation({
    onPipelineEditorCreated: editPipeline.onPipelineEditorCreated,
    addNewParticipants: (participants) => {
      const created = participants[0];
      if (!created) return;
      void attach({
        entity_name: created.entity_name,
        entity_meta: created.entity_meta,
        entity_settings: created.entity_settings,
      });
    },
  });

  return { editPipeline, pipelineCreation };
}

/**
 * Toolkit editing is opened from the conversation participant rail. The page
 * normalizes the wire participant and calls `handleShowToolkitEditor`, which
 * enters the same editor mutex used by agent and pipeline editors. MCP
 * participants use this path too because they are toolkit participants with
 * `meta.mcp` set.
 */
function useToolkitEditing(isEditingToolkit: boolean, attach: AttachCreatedParticipant) {
  const setToolkitEditingBlockNav = useCallback((blocked: boolean) => {
    useEditorStateStore.getState().setEditingToolkit(blocked);
    useNavBlockerStore.getState().setBlockNav(blocked, NAV_BLOCK_WARNING);
  }, []);
  // Baseline: a Redux `isToolkitCreateMode` flag distinct from
  // `isEditingToolkit`. No consumer anywhere else in this worktree reads an
  // equivalent flag (`useEditToolkit`'s OWN `isToolkitCreateMode` return
  // value already tracks this locally for its own callers) — a disclosed
  // no-op, not a silently-dropped write.
  const setToolkitCreateMode = useCallback((_creating: boolean) => {}, []);

  const editToolkit = toolkitEditorHooks.useEditToolkit({
    navBlocker: { isEditingToolkit, setToolkitEditingBlockNav, setToolkitCreateMode },
  });

  // `addNewParticipants` is real now (issue #867) — posts the freshly
  // created toolkit onto the current conversation via `attach`, the same
  // path `useAgentEditing` uses. Toolkits are never activated (they are
  // tools, not conversational entities — `useToolkitCreation`'s own doc
  // comment), so there is no `onSetActiveParticipant`/`onAdded` callback to
  // honour here at all, unlike the agent wrapper.
  const toolkitCreation = toolkitEditorHooks.useToolkitCreation({
    onToolkitEditorCreated: editToolkit.onToolkitEditorCreated,
    addNewParticipants: async (participants) => {
      const created = participants[0];
      if (!created) return;
      await attach({
        entity_name: ChatParticipantType.Toolkits,
        entity_meta: { id: created.id },
        ...(created.version_details?.id !== undefined ? { entity_settings: { version_id: created.version_details.id } } : {}),
      });
    },
  });

  // The REAL create/save mutations for `<ToolkitEditor>`'s non-optional
  // `deps.createToolkit`/`deps.saveToolkit`. These used to be a
  // reject-with-"no backend endpoint yet" stub — stale since Phase 1c made
  // both generated operations real (`features/toolkits/api/toolkits.ts`'s
  // CORRECTION; `pages/toolkits` already saves through the same hooks), so
  // every toolkit create/save FROM CHAT failed by construction.
  const createToolkit = useToolkitCreate();
  const saveToolkit = useToolkitEdit();
  const toolkitWriteDeps = useMemo(() => ({ createToolkit, saveToolkit }), [createToolkit, saveToolkit]);

  return { editToolkit, toolkitCreation, toolkitWriteDeps };
}

export interface ChatWithEditorsWiring {
  readonly isEditingAgent: boolean;
  readonly isEditingPipeline: boolean;
  readonly isEditingToolkit: boolean;
  /**
   * `editAgent.editingAgent`, re-derived through `readAgentParticipantSnapshot`
   * — NOT the same value passed straight through. `useEditAgent`'s own
   * `EditAgentParticipant`/`EditAgentParticipantSettings` declare their
   * optional fields WITH an explicit `| undefined` (e.g. `version_id?:
   * string | number | undefined`); `AgentEditor`'s `agent` prop
   * (`AgentEditorAgentLike`) declares the same fields WITHOUT it. Under
   * `exactOptionalPropertyTypes` these are genuinely incompatible types —
   * verified directly (`npx tsc --noEmit` rejects `editAgent.editingAgent`
   * passed as-is to `agent`) — even though `Pipeline`'s/`Toolkit`'s
   * equivalent state objects have no such mismatch (their own hooks never
   * added the explicit `| undefined`). Re-decoding through
   * `readAgentParticipantSnapshot` (the SAME defensive `typeof`-narrowing
   * function built for the mutex's queue round-trip) rebuilds a clean,
   * `exactOptionalPropertyTypes`-safe object — and, as a side benefit,
   * recovers `entity_meta.project_id`/`meta.name` (fields
   * `EditAgentParticipant` itself never declares but `AgentEditorAgentLike`
   * needs for correct public-agent detection/title display) because they
   * are still genuinely present on the underlying object at runtime — it
   * was built by this same file's own `toAgentParticipantSnapshot` when the
   * editor was opened, see `useAgentEditing`'s own `onShowAgentEditor` call
   * site below.
   */
  readonly agentForEditor: ReturnType<typeof readAgentParticipantSnapshot>;
  readonly editAgent: ReturnType<typeof useAgentEditing>['editAgent'];
  readonly agentCreation: ReturnType<typeof useAgentEditing>['agentCreation'];
  readonly editPipeline: ReturnType<typeof usePipelineEditing>['editPipeline'];
  readonly pipelineCreation: ReturnType<typeof usePipelineEditing>['pipelineCreation'];
  readonly editToolkit: ReturnType<typeof useToolkitEditing>['editToolkit'];
  readonly toolkitCreation: ReturnType<typeof useToolkitEditing>['toolkitCreation'];
  /** The real generated create/save mutations for `<ToolkitEditor deps>` — see `useToolkitEditing`'s own comment. */
  readonly toolkitWriteDeps: ReturnType<typeof useToolkitEditing>['toolkitWriteDeps'];
  /** The canvas editor's open/close state and the ref the mutex saves through — see `useCanvasEditing`. */
  readonly canvas: ReturnType<typeof useCanvasEditing>;
  readonly mutex: ReturnType<typeof useEditorMutex>;
  readonly handleShowAgentEditor: (participant: Participant) => void;
  readonly handleShowPipelineEditor: (participant: Participant) => void;
  readonly handleShowToolkitEditor: (participant: Participant) => void;
  /** Opens the canvas editor for a stored canvas block in the transcript (issue 853), through the mutex. */
  readonly handleShowCanvasEditor: (payload: CanvasEditPayload) => void;
}

/**
 * Composes every editor's hooks + `useEditorMutex` into one wiring object.
 * The actual mutual-exclusion logic lives entirely in `useEditorMutex`
 * itself (unmodified, imported from `../model/useEditorMutex`) — this hook
 * only supplies its real open/close callback pairs.
 *
 * **Canvas is now REAL.** It used to be an inert stub here —
 * `onShowCanvasEditor` a no-op and `canvasEditorRef.current` permanently
 * `null`, on the grounds that `CanvasEditor.tsx` had no composition point.
 * It has one now (`useCanvasEditing` + the `<CanvasEditor>` in
 * `ChatWithEditors.tsx`), which also un-breaks `useEditorMutex`'s
 * `closeHandlers.isEditingCanvas = () => canvasEditorRef.current?.save?.()`
 * — that branch is what saves an open canvas before another editor replaces
 * it, and against a null ref it discarded the edits silently.
 *
 * **Artifact is still a DISCLOSED, INERT stub.** `useEditorMutex` requires
 * `onShowArtifactEditor`/`onCloseArtifactEditor` as non-optional params, and
 * no `ArtifactEditor` component exists in this app at all — there is nothing
 * to mount. Both remain no-ops.
 */
export interface UseChatWithEditorsParams {
  /** The project the current conversation belongs to — needed to POST a freshly-created entity as a participant (issue #867's `attach`). */
  readonly projectId: string | undefined;
  /** The current conversation's id, `undefined` for a still-draft (unsent) chat — see `useAttachCreatedParticipant`'s own doc comment for what that means for a create-from-chat attach. */
  readonly conversationId: string | undefined;
}

export function useChatWithEditors({ projectId, conversationId }: UseChatWithEditorsParams): ChatWithEditorsWiring {
  const isEditingAgent = useEditorStateStore((s) => s.isEditingAgent);
  const isEditingPipeline = useEditorStateStore((s) => s.isEditingPipeline);
  const isEditingToolkit = useEditorStateStore((s) => s.isEditingToolkit);

  const attach = useAttachCreatedParticipant(projectId, conversationId);

  const { editAgent, agentCreation } = useAgentEditing(isEditingAgent, attach);
  const { editPipeline, pipelineCreation } = usePipelineEditing(isEditingPipeline, attach);
  const { editToolkit, toolkitCreation, toolkitWriteDeps } = useToolkitEditing(isEditingToolkit, attach);

  const canvas = useCanvasEditing();
  const { canvasEditorRef, onShowCanvasEditor } = canvas;
  const onShowArtifactEditor = useCallback((_info: EditorOpenInfo) => {}, []);
  const onCloseArtifactEditor = useCallback(() => {}, []);

  const onShowAgentEditor = useCallback(
    (info: EditorOpenInfo) => {
      const snapshot = readAgentParticipantSnapshot(info);
      if (snapshot) editAgent.onShowAgentEditor(snapshot);
    },
    [editAgent],
  );
  const onShowPipelineEditor = useCallback(
    (info: EditorOpenInfo) => {
      const snapshot = readPipelineParticipantSnapshot(info);
      if (snapshot) editPipeline.onShowPipelineEditor(snapshot);
    },
    [editPipeline],
  );
  const onShowToolkitEditor = useCallback(
    (info: EditorOpenInfo) => {
      const snapshot = readToolkitParticipantSnapshot(info);
      if (snapshot) editToolkit.onShowToolkitEditor(snapshot);
    },
    [editToolkit],
  );

  const mutex = useEditorMutex({
    onShowAgentEditor,
    onCloseAgentEditor: editAgent.onCloseAgentEditor,
    onShowToolkitEditor,
    onCloseToolkitEditor: editToolkit.onCloseToolkitEditor,
    onShowPipelineEditor,
    onClosePipelineEditor: editPipeline.onClosePipelineEditor,
    onShowCanvasEditor,
    canvasEditorRef,
    onShowArtifactEditor,
    onCloseArtifactEditor,
    onShowAgentEditorCreator: editAgent.onShowAgentEditorCreator,
    onShowToolkitEditorCreator: editToolkit.onShowToolkitEditorCreator,
    onShowPipelineEditorCreator: editPipeline.onShowPipelineEditorCreator,
  });

  // The producer half: `NewChatInput`'s `onShowAgentEditor`/
  // `onShowPipelineEditor` (via `ChatBox`/`ChatPage`) call these with a
  // real `Participant` — encoded into `EditorOpenInfo` via a fresh object
  // literal (assignable to `Record<string, unknown>`; the typed snapshot
  // itself is not — see `../lib/editorParticipantAdapters.ts`'s own doc
  // comment) and handed to the mutex, which opens immediately or queues.
  const handleShowAgentEditor = useCallback(
    (participant: Participant) => {
      mutex.onEditAgent({ ...toAgentParticipantSnapshot(participant) });
    },
    [mutex],
  );
  const handleShowPipelineEditor = useCallback(
    (participant: Participant) => {
      mutex.onEditPipeline({ ...toPipelineParticipantSnapshot(participant) });
    },
    [mutex],
  );
  const handleShowToolkitEditor = useCallback(
    (participant: Participant) => {
      mutex.onEditToolkit({ ...toToolkitParticipantSnapshot(participant) });
    },
    [mutex],
  );

  /*
   * The transcript's canvas opener (issue 853). It goes through the MUTEX,
   * not straight to `useCanvasEditing`: a reader who has the agent editor
   * open and then clicks a canvas must get the same "another editor is open"
   * confirm every other pair already gets, and `onEditCanvas` is the branch
   * that raises it. The first argument is the message the canvas belongs to,
   * which nothing on this path reads (`toSelectedCodeBlockInfo` drops it) —
   * it is passed as `undefined` rather than invented.
   */
  const handleShowCanvasEditor = useCallback(
    (payload: CanvasEditPayload) => {
      mutex.onEditCanvas(undefined, payload);
    },
    [mutex],
  );

  // See `ChatWithEditorsWiring.agentForEditor`'s own doc comment for why
  // this re-decode (rather than passing `editAgent.editingAgent` straight
  // through) is necessary. `{ ...editAgent.editingAgent }` first, because a
  // concretely-typed value (not a fresh object literal) is not assignable
  // to `EditorOpenInfo`'s `Record<string, unknown>` without this spread —
  // same reasoning as `handleShowAgentEditor` below; spreading `null`
  // (`isEditingAgent`/`isCreateMode` both false) is valid JS, evaluates to
  // `{}`, and `readAgentParticipantSnapshot` safely returns `undefined` for it.
  const agentForEditor = readAgentParticipantSnapshot({ ...editAgent.editingAgent });

  return {
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
    canvas,
    mutex,
    handleShowAgentEditor,
    handleShowPipelineEditor,
    handleShowToolkitEditor,
    handleShowCanvasEditor,
  };
}
