/**
 * `/chat` page — real composition root wiring, replacing the Phase-3/4
 * scaffold. Mirrors the old app's `ChatWrapper` (`apps/elitea-ui/src/pages/
 * ChatWrapper.jsx`): the SAME component renders both `/chat` and
 * `/chat/:conversationId` (a new, unsaved conversation vs. an existing
 * one), reading the optional `conversationId` param itself rather than
 * being two exclusive screens — see `src/routes/_shell/chat.tsx`'s own
 * header comment for the TanStack-side reasoning.
 *
 * Real data (`useChatPageData`) + real active-participant selection
 * (persisted via `chat-participants`' `useLocalActiveParticipant`, same
 * mechanism the old app used) feed the C6 `ChatBox` composition root,
 * which already owns everything downstream (streaming, HITL, message
 * list, input). See `useChatPageData.ts`'s own doc comment for the one
 * disclosed data gap (`projectId` has no fully-wired source until unit
 * S1/AppShell or R2/router-context lands).
 *
 * A successful first send promotes the created conversation into the route.
 * This preserves one conversation UUID for later turns and durable history.
 *
 * `editorCallbacks` (unit A2/A4/C6-editor-composition follow-up): an
 * OPTIONAL prop bundle — same "group related props into one slot" §3.5
 * convention `widgets/chat-box/ui/ChatBox.tsx`'s own `user`/`llm`/`onDelete`
 * props already use — forwarded straight through to `ChatBox`'s matching
 * optional prop. Backward-compatible additive change: `ChatPage` took zero
 * props before this, so every existing `<ChatPage />` call site (just
 * `src/routes/_shell/chat.tsx`, until `processes/chat/ui/ChatWithEditors.tsx`
 * landed) keeps compiling unchanged. See `ChatWithEditors.tsx`'s own module
 * doc comment for who actually supplies real (non-no-op) callbacks here.
 */
import type { ComponentProps } from 'react';
import { memo, useCallback, useEffect, useState } from 'react';
import { useNavigate, useParams, useSearch } from '@tanstack/react-router';
import { useQueryClient } from '@tanstack/react-query';

import Box from '@mui/material/Box';

import { conversationNavigation, useChatSessionStore } from '@/entities/conversation';
import { useDeleteParticipantMutation, type Participant } from '@/entities/participant';
import { AddNewUserModal, canParticipantBeActiveInChat, ParticipantsWrapper, useLocalActiveParticipant } from '@/features/chat-participants';
import type { ChatBoxProps } from '@/widgets/chat-box';
import { ChatBox, toParticipant } from '@/widgets/chat-box';
import { ContextBudget } from '@/widgets/context-budget';

import { useChatPageData } from './useChatPageData';
import { useChatModelSettings } from './useChatModelSettings';
import { useAddParticipants } from './useAddParticipants';

/**
 * Baseline `rightPanelWidth` (`NewChat.jsx:187`). `ParticipantsWrapper`'s own
 * default is 320, which is not the production width.
 */
const PARTICIPANTS_PANEL_WIDTH = 276;

/** The two deep-link search params a notification href carries — `src/features/notifications/lib/routes.ts`'s `chatHref`. */
interface ChatDeepLinkSearch {
  readonly conversation?: string;
  readonly message_id?: string;
}

/**
 * Resolves the conversation from the path param OR from the
 * `?conversation=` search param, then canonicalises the URL.
 *
 * A notification link (`chatHref`) points at
 * `/{projectId}/chat?conversation=<id>&message_id=<id>`. The project splat
 * strips the project segment and lands on `/chat?conversation=<id>`, which
 * has NO path param. Reading only the path param therefore opened an empty
 * new chat. `resolveConversationIdFromUrl` is the ported baseline rule:
 * the path param wins, the search param is the fallback.
 *
 * After the fallback resolves, this replaces the URL with the canonical
 * `/chat/<id>` form. It also drops the consumed `conversation` param. A later
 * in-page navigation therefore cannot restore a stale conversation. TanStack drops
 * every search param unless the caller returns them, so `message_id` is
 * carried over explicitly here.
 */
function useDeepLinkedConversationId(routeConversationId: string | undefined): { readonly conversationId: string | undefined; readonly messageId: string | undefined } {
  const navigate = useNavigate();
  const search = useSearch({ strict: false }) as ChatDeepLinkSearch;
  const conversationId = conversationNavigation.resolveConversationIdFromUrl(routeConversationId, search.conversation);
  const messageId = search.message_id || undefined;

  useEffect(() => {
    if (routeConversationId || !conversationId) return;
    void navigate({ to: '/chat/$conversationId', params: { conversationId }, search: (prev: ChatDeepLinkSearch) => ({ ...prev, conversation: undefined }), replace: true });
  }, [navigate, routeConversationId, conversationId]);

  return { conversationId, messageId };
}

/**
 * Publishes the deep link's `?message_id=` into `chatSessionStore`, which is
 * what `ChatMessageList`/`useHighlightUserMessage` scroll to and highlight.
 * Both of those consumers existed already. Nothing ever wrote a real id into
 * the store. The "jump to the message you were mentioned in" half of a
 * notification link therefore did nothing. Mirrors the baseline `NewChat.jsx`.
 */
function useMessageIdToView(messageId: string | undefined, loadedConversationId: string | undefined): void {
  useEffect(() => {
    if (!messageId || !loadedConversationId) return;
    useChatSessionStore.getState().setMessageIdToView(messageId);
  }, [messageId, loadedConversationId]);
}

/**
 * The conversation's participant rows, for the add-participant picker's
 * already-added exclusion.
 *
 * `ChatBoxActiveConversation` types them as `unknown[]` while the picker reads
 * each row's `entity_name`/`entity_meta` — the same rows, described at two
 * precisions by two slices. Module scope, not inline: `ChatPage` is on its
 * §3.5 complexity ceiling and this is two more branches there.
 */
function participantRows(activeConversation: { readonly participants?: readonly unknown[] } | undefined): readonly Record<string, unknown>[] {
  return (activeConversation?.participants ?? []) as readonly Record<string, unknown>[];
}

function conversationIdOf(activeConversation: unknown): string | undefined {
  return (activeConversation as { readonly id?: string } | undefined)?.id;
}

export function findActiveParticipantById(participants: readonly unknown[] | undefined, id: string | undefined): unknown {
  if (!id) return undefined;
  return participants?.find((raw) => {
    const participant = raw as { readonly id?: string; readonly entity_name?: string } | null;
    return participant?.id === id && canParticipantBeActiveInChat(participant);
  });
}

/** @public The agent/pipeline editor open/close callbacks `ChatPage` forwards to `ChatBox` — see this module's own doc comment. */
export interface ChatEditorCallbacks {
  readonly onShowAgentEditor?: (participant: Participant) => void;
  readonly onShowPipelineEditor?: (participant: Participant) => void;
  readonly onShowToolkitEditor?: (participant: Participant) => void;
  readonly onCloseAgentEditor?: () => void;
  readonly onClosePipelineEditor?: () => void;
}

/** @public */
export interface ChatPageProps {
  readonly editorCallbacks?: ChatEditorCallbacks;
  /**
   * Real entity lists for the composer's "+" menu, forwarded straight to
   * `ChatBox`. Supplied by `processes/chat/ui/ChatWithEditors.tsx`, which is
   * the only layer allowed to call `useChatEntityBrowser` — see `ChatBox`'s
   * own prop doc for why the data cannot be fetched further down.
   */
  readonly entitySubmenus?: NonNullable<ChatBoxProps['extensions']>['entitySubmenus'];
}

const ChatPage = memo(({ editorCallbacks, entitySubmenus }: ChatPageProps) => {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { conversationId: routeConversationId } = useParams({ strict: false }) as { conversationId?: string };
  const { conversationId, messageId } = useDeepLinkedConversationId(routeConversationId);
  const { projectId, user, activeConversation, isLoadingConversation } = useChatPageData({ conversationId });
  const llm = useChatModelSettings({ activeConversation, projectId, userId: user?.id });
  const { getLocalActiveParticipant, setLocalActiveParticipant, clearLocalActiveParticipant } = useLocalActiveParticipant();
  const { mutate: deleteParticipant } = useDeleteParticipantMutation();
  useMessageIdToView(messageId, conversationIdOf(activeConversation));

  const [activeParticipant, setActiveParticipant] = useState<unknown>(undefined);
  // Baseline default: collapsed (`NewChat.jsx:166`).
  const [participantsCollapsed, setParticipantsCollapsed] = useState(true);
  // The panel's add-participant picker — see `useAddParticipants` for what
  // was missing and why `open` can be `undefined`.
  const addParticipants = useAddParticipants({ projectId, conversationId });

  /**
   * A conversation is written by the FIRST SEND, not by a button
   * (`useChatBoxSend.ts`'s `createConversationForSend`), so this callback is
   * the only place the app learns that the rail's listing is now out of date.
   *
   * The rail reads `folderApi.useList`, a cached query nothing else
   * invalidates: every folder mutation invalidates `['folder', 'list']`, but
   * no conversation one did. So a conversation created by a send appeared in
   * the route and in the transcript and was simply missing from the rail
   * beside it until something else happened to refetch. The baseline does the
   * same thing at the same layer (`NewChat.jsx:459`,
   * `invalidateTags([TAG_TYPE_FOLDERS])`).
   *
   * Invalidate, not refetch: the key already holds data, so the listing keeps
   * showing while it revalidates instead of collapsing to skeletons.
   */
  const handleConversationCreated = useCallback(
    (created: { readonly id?: string | number }) => {
      if (created.id === undefined) return;
      void queryClient.invalidateQueries({ queryKey: ['folder', 'list'] });
      void navigate({ to: '/chat/$conversationId', params: { conversationId: String(created.id) } });
    },
    [navigate, queryClient],
  );

  // Restore the conversation's last-active participant once its real
  // participant list has loaded (baseline: `ChatWrapper.jsx`'s own
  // mount-time `getLocalActiveParticipant` read).
  useEffect(() => {
    if (!conversationId || !activeConversation?.participants?.length) return;
    // `useLocalActiveParticipant` is `@ts-nocheck` (see that file) — its
    // exports are untyped (`any`) from this call site's perspective.
    const local = getLocalActiveParticipant(conversationId) as { readonly participantId?: string };
    const found = findActiveParticipantById(activeConversation.participants, local.participantId);
    setActiveParticipant(found);
    // Only re-run when the conversation identity or its participant list changes.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [conversationId, activeConversation?.participants]);

  const handleChangeParticipant = (participant: unknown) => {
    setActiveParticipant(participant);
    const id = (participant as { readonly id?: string } | null)?.id;
    if (conversationId && id) setLocalActiveParticipant(conversationId, id);
  };

  const handleDeleteParticipant = useCallback(
    (participant: Record<string, unknown>) => {
      const participantId = participant.id;
      const hasParticipantId = typeof participantId === 'string' || typeof participantId === 'number';
      if (projectId === undefined || conversationId === undefined || !hasParticipantId) return;

      deleteParticipant(
        { projectId, conversationId, id: String(participantId) },
        {
          onSuccess: () => {
            if ((activeParticipant as { readonly id?: unknown } | undefined)?.id !== participantId) return;
            setActiveParticipant(undefined);
            clearLocalActiveParticipant(conversationId);
          },
        },
      );
    },
    [activeParticipant, clearLocalActiveParticipant, conversationId, deleteParticipant, projectId],
  );

  const handleEditParticipant = useCallback(
    (participant: Record<string, unknown>) => {
      const normalized = toParticipant(participant);
      if (!normalized) return;
      const editorType = normalized.entitySettings?.agentType === 'pipeline' ? 'pipeline' : normalized.entityName;
      const handlers = {
        application: editorCallbacks?.onShowAgentEditor,
        pipeline: editorCallbacks?.onShowPipelineEditor,
        toolkit: editorCallbacks?.onShowToolkitEditor,
      };
      handlers[editorType as keyof typeof handlers]?.(normalized);
    },
    [editorCallbacks],
  );

  return (
    <Box sx={{ display: 'flex', height: '100%', minHeight: 0, width: '100%' }}>
      <Box sx={{ flexGrow: 1, minWidth: 0, height: '100%' }}>
        <ChatBox
          conversation={{
            ...(activeConversation ? { active: activeConversation } : {}),
            isLoading: isLoadingConversation,
            onCreated: handleConversationCreated,
          }}
          {...(projectId !== undefined ? { projectId } : {})}
          {...(user ? { user } : {})}
          llm={{ settings: llm.settings, onSetSettings: llm.onSetSettings }}
          participant={{ active: activeParticipant, onChange: handleChangeParticipant }}
          {...(editorCallbacks || entitySubmenus
            ? {
                extensions: {
                  ...(editorCallbacks ? { editorCallbacks } : {}),
                  ...(entitySubmenus ? { entitySubmenus } : {}),
                },
              }
            : {})}
        />
      </Box>
      {/*
        * The participants rail. `features/chat-participants` was complete —
        * wrapper, layout, expanded list, collapsed strip, collapsed dropdown,
        * the collapse chevron, all tested — and had ZERO call sites, so `/chat`
        * rendered the conversation rail and the chat box and nothing on the
        * right. `ChatWithEditors`'s own comment already said the chat column
        * runs to the edge "so the participants rail can dock there"; nothing
        * docked.
        *
        * Mounted HERE rather than in `ChatWithEditors` because the rail needs
        * `activeConversation` and `activeParticipant`, and this is the
        * component that owns both. Mounting a layer up would mean lifting that
        * state out of the page for no other reason.
        *
        * Default COLLAPSED, matching the baseline (`NewChat.jsx:166`
        * `collapsedParticipants` starts `true`) — which is why production shows
        * the `»` chevron at the top right on load rather than an open panel.
        */}
      <ParticipantsWrapper
        collapsed={participantsCollapsed}
        onCollapsed={() => setParticipantsCollapsed((prev) => !prev)}
        panelWidth={PARTICIPANTS_PANEL_WIDTH}
        {...(activeConversation
          // `ChatBoxActiveConversation` types `participants` as `unknown[]`,
          // the rail's own prop as `Record<string, unknown>[]`. Same rows, two
          // slices of the app describing them at different precision; the cast
          // is the boundary, not a claim about the data.
          ? { activeConversation: activeConversation as NonNullable<ComponentProps<typeof ParticipantsWrapper>['activeConversation']> }
          : {})}
        {...(activeParticipant ? { activeParticipant: activeParticipant as Record<string, unknown> } : {})}
        onSelectParticipant={handleChangeParticipant}
        onDeleteParticipant={handleDeleteParticipant}
        onEditParticipant={handleEditParticipant}
        onAddParticipants={addParticipants.open}
        /*
         * The context-budget panel. `ParticipantsWrapper` has always accepted
         * this slot (and gates the `conversationId` it hands over on the
         * conversation being neither new nor playback); nothing supplied it, so
         * the foot of the rail was empty. `features/` may not import
         * `widgets/`, which is why the slot is filled HERE — the page is the
         * lowest layer allowed to name the widget. `projectId` comes from
         * `useChatPageData`, which already resolves it for `ChatBox`.
         */
        renderContextBudget={({ conversationId: budgetConversationId, collapsed: budgetCollapsed }) => (
          <ContextBudget
            conversationId={activeConversation?.isNew || activeConversation?.isPlayback ? undefined : budgetConversationId}
            projectId={projectId}
            collapsed={budgetCollapsed ?? false}
          />
        )}
      />
      <AddNewUserModal
        open={addParticipants.isOpen}
        onClose={addParticipants.close}
        onAddUsers={addParticipants.addUsers}
        participants={participantRows(activeConversation)}
      />
    </Box>
  );
});

ChatPage.displayName = 'ChatPage';

export default ChatPage;
