/**
 * Issue 940/A6 — Chat "Duplicate" action, for the conversation rail
 * (`useConversationRail`). It resolves to the new conversation's id; the
 * caller opens it.
 *
 * No dedicated Go clone route exists
 * (`services/elitea-main/internal/api/v2/conversations/handler.go`'s
 * `Create` reads only `name`/`meta`/`author_id` off the request body —
 * confirmed by reading it directly), so this composes the copy client-side
 * from three routes that already exist and are already wired into this app:
 * `GET .../conversation/...` (participants + settings + visibility),
 * `POST .../conversations/...` (create), and `POST .../participants/...`
 * (`useAddParticipantMutation`, the same mutation `useAttachCreatedParticipant`
 * next door already posts through).
 *
 * Per-case behaviour (ELITEA-2616..2624):
 *  - The duplicate starts as an empty conversation — no `chat_history` is
 *    ever read or copied, so independence (2618) is structural, not a rule
 *    this function enforces.
 *  - Pin state resets for free: pinning is a separate entity-pin mapping
 *    (`usePinConversation.hooks.ts`), never copied here, so a new
 *    conversation id starts unpinned regardless of the original (2620).
 *  - Public visibility (`is_private`) IS carried over: a public original
 *    produces a public duplicate, via the exact same `conversationApi.edit
 *    is_private:false` PUT the row menu's own "Make public" action uses
 *    (2621-2623) — the Go route defaults every newly-created conversation
 *    private, so this is a second call, not a create-time flag.
 *  - Participants (users + every AI type) are read back from the
 *    original's own detail response and re-POSTed verbatim
 *    (`entity_name`/`entity_meta`/`entity_settings`) — the participant
 *    row's own `id` is dropped, so the duplicate gets its OWN participant
 *    mapping rows rather than reusing the original's (2617, 2624:
 *    duplicating never exposes or moves data across projects — both the
 *    read and every write below stay scoped to the SAME `projectId` the
 *    menu row was rendered for).
 *  - The rail's own list query is invalidated (not patched in place) after
 *    a successful duplicate, so the new row's real sort position (issue
 *    #6271: newest first) comes from the server's own `updated_at` order,
 *    the same source of truth every other row on the rail already trusts.
 */
import { useCallback } from 'react';

import { useQueryClient } from '@tanstack/react-query';

import { clipToLimit, NAME_MAX_LENGTH } from '@elitea/client-core/limits';

import type { Conversation } from '@/entities/conversation';
import { conversationApi } from '@/entities/conversation';
import { useAddParticipantMutation } from '@/entities/participant';

/**
 * The prefix `entities/folder/api/foldersApi.ts`'s own folder-list query key
 * always starts with (`FOLDER_QUERY_ROOT` there, `['folder', 'list', ...]`)
 * — not exported by that module (only the curated `folderApi` bundle is), so
 * restated here rather than reached into. `queryClient.invalidateQueries`
 * matches by PREFIX, so this one root key invalidates every
 * `projectId`/`params` variant `useQueryFoldersList` holds, the same way
 * `foldersApi.ts`'s own create/update/remove mutations already invalidate
 * their sibling queries.
 */
const FOLDER_LIST_QUERY_KEY = ['folder', 'list'] as const;

const COPY_SUFFIX = ' (copy)';

/**
 * The duplicate's name: the source's with the suffix, clipped so the whole stays inside the
 * server's name limit (issue 1968). Without the clip a source near the limit could not be
 * duplicated at all.
 */
export function duplicateConversationName(sourceName: string): string {
  return `${clipToLimit(sourceName, NAME_MAX_LENGTH - COPY_SUFFIX.length)}${COPY_SUFFIX}`;
}

export function useDuplicateConversation(projectId: string | undefined, toastError: (message: string) => void): (conversation: Conversation) => Promise<string | undefined> {
  const queryClient = useQueryClient();
  const { mutateAsync: addParticipants } = useAddParticipantMutation();

  return useCallback(
    async (conversation: Conversation): Promise<string | undefined> => {
      if (projectId === undefined) return undefined;
      try {
        const source = await conversationApi.details({ projectId, id: conversation.id });
        const duplicateName = duplicateConversationName(source.name || conversation.name);
        const isPrivate = source.is_private !== false;

        const created = await conversationApi.create({
          projectId,
          name: duplicateName,
          is_private: true,
          ...(source.meta !== undefined ? { meta: source.meta } : {}),
        });

        if (!isPrivate) {
          await conversationApi.edit({ projectId, id: created.id, is_private: false });
        }

        const participantsToCopy = (source.participants ?? []).flatMap((participant) =>
          typeof participant.entity_name === 'string'
            ? [
                {
                  entity_name: participant.entity_name,
                  ...(participant.entity_meta !== undefined ? { entity_meta: participant.entity_meta } : {}),
                  ...(participant.entity_settings !== undefined ? { entity_settings: participant.entity_settings } : {}),
                },
              ]
            : [],
        );
        if (participantsToCopy.length > 0) {
          await addParticipants({ projectId, conversationId: String(created.id), participants: participantsToCopy });
        }

        void queryClient.invalidateQueries({ queryKey: FOLDER_LIST_QUERY_KEY });

        return String(created.id);
      } catch {
        toastError('Failed to duplicate the conversation');
        return undefined;
      }
    },
    [projectId, addParticipants, queryClient, toastError],
  );
}
