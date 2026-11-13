import { useCallback, useEffect, useMemo, useRef, useState } from 'react';

import { WELCOME_MESSAGE_ID } from '@/shared/lib/enums';
import type { Participant } from '@/entities/participant';

import { usePipelineAttachments } from './usePipelineAttachments';
import type { UsePipelineAttachmentsResult } from './usePipelineAttachments';
import { buildPipelineParticipant, getInitialChatHistory, getWelcomeMessage } from './pipelineChat.helpers';
import type { ChatConversation, ChatHistoryMessage, ChatPipelineVersionDetails, ChatSource } from './pipelineChat.types';
import { NAME_MAX_LENGTH, clipToLimit } from '@/shared/lib/textLimits';

/**
 * Conversation-lifecycle slice of `usePipelineChat` — restore-by-id,
 * create-on-mount, participant sync on version change, welcome-message
 * injection/removal, and the `chat_history` state setter. Split out of
 * `usePipelineChat.hooks.ts` purely to keep every function under this
 * codebase's `complexity`/`max-lines` gates — mirrors
 * `features/agents/lib/hooks/useApplicationChatConversation.hooks.ts` for
 * the sibling baseline file (`usePipelineChat.hooks.js` and
 * `useApplicationChat.hooks.js` share this exact conversation-lifecycle
 * shape in the baseline).
 *
 * **Correction against the sibling port, restoring real baseline
 * behaviour:** `pipelineParticipant` is wrapped in `useMemo` below.
 * `useApplicationChatConversation.hooks.ts`'s `applicationParticipant` is
 * NOT memoised (`buildApplicationParticipant(...)` called directly in the
 * render body) — but the actual baseline, `usePipelineChat.hooks.js:99-124`,
 * DOES memoise it (`useMemo(() => {...}, [pipelineId, pipelineName,
 * pipelineVersionDetails, projectId])`), and this file's own
 * `useSyncParticipantEffect` genuinely depends on that: its effect fires on
 * `[pipelineParticipant, setActiveConversation]`, and once an active
 * conversation already contains an `entityName: 'application'` participant,
 * its body unconditionally returns a NEW conversation object (`{...prev,
 * participants: prev.participants.map(...)}`) on every run. An unmemoised
 * `pipelineParticipant` is a new object every render -> the effect re-fires
 * every render -> `setActiveConversation` produces a new object every time
 * -> a genuine infinite render loop (reproduced directly: this file's own
 * `usePipelineChatConversation.hooks.test.tsx` OOM-crashed the test worker
 * before this fix). Memoising restores the real baseline's own semantics
 * and fixes the loop; not a stylistic deviation.
 */
export interface UsePipelineChatConversationParams {
  readonly pipelineId: string | number | undefined;
  readonly pipelineName: string | undefined;
  readonly pipelineVersionDetails: ChatPipelineVersionDetails | undefined;
  readonly projectId: string | undefined;
  readonly source: ChatSource;
  readonly restoredConversationID: string | number | null;
  readonly restoredConversationData: ChatConversation | undefined;
  readonly isLoadingRestoredConversation: boolean;
  readonly isErrorRestoredConversation: boolean;
  readonly onRestoreConversationComplete: () => void;
  readonly onInfo?: ((message: string) => void) | undefined;
  readonly onError?: ((message: string) => void) | undefined;
}

export interface UsePipelineChatConversationResult extends UsePipelineAttachmentsResult {
  readonly activeConversation: ChatConversation | null;
  readonly activeParticipant: Participant | null;
  readonly setActiveConversation: (
    update: ChatConversation | null | ((prev: ChatConversation | null) => ChatConversation | null),
  ) => void;
  readonly setActiveParticipant: (update: Participant | null | ((prev: Participant | null) => Participant | null)) => void;
  readonly isCreatingConversation: boolean;
  readonly pipelineParticipant: Participant | null;
  readonly chatHistoryRef: React.RefObject<ChatHistoryMessage[]>;
  readonly setChatHistory: (
    update: ChatHistoryMessage[] | ((prev: ChatHistoryMessage[]) => ChatHistoryMessage[]),
  ) => void;
}

function useRestoreConversationEffect(
  params: UsePipelineChatConversationParams,
  setActiveConversation: (c: ChatConversation) => void,
  setActiveParticipant: (p: Participant) => void,
  chatHistoryRef: React.RefObject<ChatHistoryMessage[]>,
  setHasRestoredConversation: (v: boolean) => void,
): void {
  const {
    restoredConversationID,
    restoredConversationData,
    isLoadingRestoredConversation,
    isErrorRestoredConversation,
    onInfo,
    onError,
    onRestoreConversationComplete,
  } = params;
  const [isRestoring, setIsRestoring] = useState(false);

  useEffect(() => {
    if (
      !restoredConversationID ||
      !restoredConversationData ||
      isLoadingRestoredConversation ||
      isErrorRestoredConversation ||
      isRestoring
    ) {
      return;
    }
    setIsRestoring(true);

    const restoredConversation: ChatConversation = { ...restoredConversationData, isPipelineChat: true };
    const pipelineParticipant = restoredConversationData.participants?.find((p) => p.entityName === 'application');

    if (pipelineParticipant) {
      setActiveConversation(restoredConversation);
      setActiveParticipant(pipelineParticipant);
      chatHistoryRef.current = restoredConversation.chat_history;
      onInfo?.('Chat restored successfully');
      setHasRestoredConversation(true);
    } else {
      onError?.('Could not find pipeline participant in restored chat');
    }

    onRestoreConversationComplete();
    setIsRestoring(false);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [restoredConversationID, restoredConversationData, isLoadingRestoredConversation, isErrorRestoredConversation, isRestoring]);

  useEffect(() => {
    if (restoredConversationID && isErrorRestoredConversation && !isLoadingRestoredConversation) {
      onError?.('Failed to restore conversation');
      setIsRestoring(false);
      onRestoreConversationComplete();
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [restoredConversationID, isErrorRestoredConversation, isLoadingRestoredConversation]);
}

function useInitializeConversationEffect(
  pipelineParticipant: Participant | null,
  activeConversation: ChatConversation | null,
  pipelineName: string | undefined,
  source: ChatSource,
  restoredConversationID: string | number | null,
  setActiveConversation: (c: ChatConversation) => void,
  setActiveParticipant: (p: Participant) => void,
  setHasRestoredConversation: (v: boolean) => void,
): boolean {
  const [isCreating, setIsCreating] = useState(false);

  useEffect(() => {
    if (!pipelineParticipant || activeConversation || isCreating || restoredConversationID) return;

    setIsCreating(true);
    setActiveConversation({
      name: clipToLimit(`Chat with ${pipelineName ?? ''}`, NAME_MAX_LENGTH),
      is_private: true,
      source,
      participants: [pipelineParticipant],
      chat_history: [],
      isNew: true,
      isPipelineChat: true,
    });
    setActiveParticipant(pipelineParticipant);
    setHasRestoredConversation(false);
    setIsCreating(false);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [pipelineParticipant, activeConversation, isCreating, pipelineName, source, restoredConversationID]);

  return isCreating;
}

function useSyncParticipantEffect(
  pipelineParticipant: Participant | null,
  setActiveConversation: (update: (prev: ChatConversation | null) => ChatConversation | null) => void,
): void {
  useEffect(() => {
    if (!pipelineParticipant) return;
    setActiveConversation((prev) => {
      if (!prev?.participants?.some((p) => p.entityName === 'application')) return prev;
      return {
        ...prev,
        participants: prev.participants.map((p) => (p.entityName === 'application' ? pipelineParticipant : p)),
      };
    });
  }, [pipelineParticipant, setActiveConversation]);
}

function useWelcomeMessageEffect(
  welcomeMessage: string | undefined,
  restoredConversationID: string | number | null,
  activeParticipantId: string | number | undefined,
  hasRestoredConversation: boolean,
  setChatHistory: (update: (prev: ChatHistoryMessage[]) => ChatHistoryMessage[]) => void,
): void {
  useEffect(() => {
    if (restoredConversationID || hasRestoredConversation) return;

    if (welcomeMessage) {
      setChatHistory((prev) => {
        const rest = prev.length && prev[0]?.id === WELCOME_MESSAGE_ID ? prev.slice(1) : prev;
        return [getWelcomeMessage(welcomeMessage, activeParticipantId ?? null), ...rest];
      });
    } else {
      setChatHistory((prev) => (prev.length && prev[0]?.id === WELCOME_MESSAGE_ID ? prev.slice(1) : prev));
    }
  }, [welcomeMessage, setChatHistory, restoredConversationID, activeParticipantId, hasRestoredConversation]);
}

export function usePipelineChatConversation(
  params: UsePipelineChatConversationParams,
): UsePipelineChatConversationResult {
  const { pipelineId, pipelineName, pipelineVersionDetails, projectId, source, restoredConversationID } = params;

  const [activeConversation, setActiveConversation] = useState<ChatConversation | null>(null);
  const [activeParticipant, setActiveParticipant] = useState<Participant | null>(null);
  const [hasRestoredConversation, setHasRestoredConversation] = useState(false);
  const chatHistoryRef = useRef<ChatHistoryMessage[]>([]);

  // Memoised — see module doc comment's "Correction against the sibling port" note.
  const pipelineParticipant = useMemo(
    () => buildPipelineParticipant(pipelineId, pipelineName, pipelineVersionDetails, projectId),
    [pipelineId, pipelineName, pipelineVersionDetails, projectId],
  );

  const { onAttachFiles, attachments, onDeleteAttachment, disableAttachments, onClearAttachments } = usePipelineAttachments({
    internalTools: pipelineVersionDetails?.meta?.internal_tools,
    versionId: pipelineVersionDetails?.id,
  });

  useRestoreConversationEffect(params, setActiveConversation, setActiveParticipant, chatHistoryRef, setHasRestoredConversation);
  const isCreatingConversation = useInitializeConversationEffect(
    pipelineParticipant,
    activeConversation,
    pipelineName,
    source,
    restoredConversationID,
    setActiveConversation,
    setActiveParticipant,
    setHasRestoredConversation,
  );
  useSyncParticipantEffect(pipelineParticipant, setActiveConversation);

  const setChatHistory = useCallback(
    (update: ChatHistoryMessage[] | ((prev: ChatHistoryMessage[]) => ChatHistoryMessage[])) => {
      setActiveConversation((prev) => {
        if (!prev) return prev;
        const nextHistory = typeof update === 'function' ? update(prev.chat_history) : update;
        chatHistoryRef.current = nextHistory;
        return { ...prev, chat_history: nextHistory };
      });
    },
    [],
  );

  useWelcomeMessageEffect(
    pipelineVersionDetails?.welcome_message,
    restoredConversationID,
    activeParticipant?.id,
    hasRestoredConversation,
    setChatHistory,
  );

  useEffect(() => {
    chatHistoryRef.current = activeConversation?.chat_history ?? [];
  }, [activeConversation?.chat_history]);

  // Resets to a fresh welcome-message-only history whenever the version id itself changes —
  // baseline's own empty-deps unmount-of-old-version effect (`usePipelineChat.hooks.js:699-704`,
  // `if (activeConversation?.chat_history?.length) resetChatHistory();`).
  //
  // **Correction against a naive port:** the guard below reads `activeConversation` via this
  // effect's own RENDER-TIME closure (exactly like the baseline's plain `if
  // (activeConversation?.chat_history?.length)` read), NOT via a `setActiveConversation(prev =>
  // ...)` functional updater. The two are NOT equivalent here: on the very first mount, this
  // effect's closure-captured `activeConversation` is still `null` (this render's own snapshot,
  // before any of THIS commit's earlier effects — `useRestoreConversationEffect`/
  // `useSyncParticipantEffect` — have had their queued `setState` calls applied), so the guard
  // correctly stays false and skips the reset, matching the baseline exactly. A functional-updater
  // `prev` instead reflects the LATEST value already queued by those earlier same-commit effects
  // (React applies queued updaters in order) — on a mount that's simultaneously restoring or
  // creating a conversation, `prev` would already be that non-empty conversation, so the guard
  // would incorrectly pass and immediately wipe the just-restored/just-created `chat_history`/`id`/
  // `uuid` back to a bare welcome message. Reproduced directly: this file's own
  // `usePipelineChatConversation.hooks.test.tsx` caught exactly this clobbering restore-then-reset
  // sequence before this fix.
  useEffect(() => {
    if (activeConversation?.chat_history.length) {
      setActiveConversation((prev) =>
        prev
          ? {
              ...prev,
              chat_history: getInitialChatHistory(pipelineVersionDetails?.welcome_message, activeParticipant?.id ?? null),
              uuid: undefined,
              id: undefined,
            }
          : prev,
      );
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [pipelineVersionDetails?.id]);

  return {
    activeConversation,
    activeParticipant,
    setActiveConversation,
    setActiveParticipant,
    isCreatingConversation,
    pipelineParticipant,
    chatHistoryRef,
    setChatHistory,
    onAttachFiles,
    attachments,
    onDeleteAttachment,
    disableAttachments,
    onClearAttachments,
  };
}
