/**
 * The toolkit half of `ChatWithEditors.hooks.ts`, in its own file for that
 * file's §3.5 400-line budget. Nothing about it changed in the move apart from
 * the attach's `entity_meta` gaining the entity NAME — see the call site.
 */
import { useCallback, useMemo } from 'react';

import { toolkitEditorHooks, useToolkitCreate, useToolkitEdit } from '@/features/toolkits';
import { ChatParticipantType } from '@/shared/lib/chat';
import { t } from '@/shared/i18n';
import { useEditorStateStore } from '@/shared/lib/editorState';
import { useNavBlockerStore } from '@/widgets/app-shell';

import type { AttachCreatedParticipant } from '../lib/useAttachCreatedParticipant';

/** The same warning the agent and pipeline editors block navigation with — restated here rather than exported from the sibling, which would be a circular import. */
const NAV_BLOCK_WARNING = t(
  'processes.chat.chatWithEditors.navBlockWarning',
  'You have unsaved changes in the editor. Are you sure you want to leave?',
);

/**
 * Toolkit editing is opened from the conversation participant rail. The page
 * normalizes the wire participant and calls `handleShowToolkitEditor`, which
 * enters the same editor mutex used by agent and pipeline editors. MCP
 * participants use this path too because they are toolkit participants with
 * `meta.mcp` set.
 */
export function useToolkitEditing(isEditingToolkit: boolean, attach: AttachCreatedParticipant) {
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
        // The NAME travels with the id (#940 A12) — same fix, same reason as
        // the agent and pipeline branches in `ChatWithEditors.hooks.ts`: a
        // toolkit created from chat drew a nameless rail row.
        entity_meta: {
          id: created.id,
          ...(created.name !== undefined ? { name: created.name } : {}),
        },
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
