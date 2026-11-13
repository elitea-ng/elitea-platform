import { useCallback, useEffect, useMemo, useRef, useState } from 'react';

import { WELCOME_MESSAGE_ID } from '@/shared/lib/enums';
import type { Participant } from '@/entities/participant';

import { useAgentAttachments } from '../useAgentAttachments';
import type { UseAgentAttachmentsResult } from '../useAgentAttachments';
import { buildApplicationParticipant, getInitialChatHistory, getWelcomeMessage } from './applicationChat.helpers';
import type { ChatApplicationVersionDetails, ChatConversation, ChatHistoryMessage, ChatSource } from './applicationChat.types';
import { NAME_MAX_LENGTH, clipToLimit } from '@/shared/lib/textLimits';

/**
 * Conversation-lifecycle slice of `useApplicationChat` — restore-by-id,
 * create-on-mount, participant sync on version change, welcome-message
 * injection/removal, and the `chat_history` state setter. Split out of
 * `useApplicationChat.hooks.ts` purely to keep every function under this
 * codebase's `complexity`/`max-lines` gates (see that file's own module doc
 * comment for the full baseline citation and disclosed-deviation list —
 * this split changes no behaviour).
 */
export interface UseApplicationChatConversationParams {
  readonly applicationId: string | number | undefined;
  readonly applicationName: string | undefined;
  readonly applicationVersionDetails: ChatApplicationVersionDetails | undefined;
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

export interface UseApplicationChatConversationResult extends UseAgentAttachmentsResult {
  readonly activeConversation: ChatConversation | null;
  readonly activeParticipant: Participant | null;
  readonly setActiveConversation: (
    update: ChatConversation | null | ((prev: ChatConversation | null) => ChatConversation | null),
  ) => void;
  readonly setActiveParticipant: (update: Participant | null | ((prev: Participant | null) => Participant | null)) => void;
  readonly isCreatingConversation: boolean;
  readonly applicationParticipant: Participant | null;
  readonly chatHistoryRef: React.RefObject<ChatHistoryMessage[]>;
  readonly setChatHistory: (
    update: ChatHistoryMessage[] | ((prev: ChatHistoryMessage[]) => ChatHistoryMessage[]),
  ) => void;
}

/** Restore-by-id effect: adopts `restoredConversationData` once it resolves. */
function useRestoreConversationEffect(
  params: UseApplicationChatConversationParams,
  setActiveConversation: (c: ChatConversation) => void,
  setActiveParticipant: (p: Participant) => void,
  chatHistoryRef: React.RefObject<ChatHistoryMessage[]>,
  setHasRestoredConversation: (v: boolean) => void,
): void {
  const { restoredConversationID, restoredConversationData, isLoadingRestoredConversation, isErrorRestoredConversation, onInfo, onError, onRestoreConversationComplete } = params;
  const [isRestoring, setIsRestoring] = useState(false);

  useEffect(() => {
    if (!restoredConversationID || !restoredConversationData || isLoadingRestoredConversation || isErrorRestoredConversation || isRestoring) {
      return;
    }
    setIsRestoring(true);

    const restoredConversation: ChatConversation = { ...restoredConversationData, isApplicationChat: true };
    const appParticipant = restoredConversationData.participants?.find((p) => p.entityName === 'application');

    if (appParticipant) {
      setActiveConversation(restoredConversation);
      setActiveParticipant(appParticipant);
      chatHistoryRef.current = restoredConversation.chat_history;
      onInfo?.('Chat restored successfully');
      setHasRestoredConversation(true);
    } else {
      onError?.('Could not find application participant in restored chat');
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

/** Create-on-mount effect: starts a fresh draft conversation once `applicationParticipant` resolves and there's nothing to restore. */
function useInitializeConversationEffect(
  applicationParticipant: Participant | null,
  activeConversation: ChatConversation | null,
  applicationName: string | undefined,
  source: ChatSource,
  restoredConversationID: string | number | null,
  setActiveConversation: (c: ChatConversation) => void,
  setActiveParticipant: (p: Participant) => void,
  setHasRestoredConversation: (v: boolean) => void,
): boolean {
  const [isCreating, setIsCreating] = useState(false);

  useEffect(() => {
    if (!applicationParticipant || activeConversation || isCreating || restoredConversationID) return;

    setIsCreating(true);
    setActiveConversation({
      name: clipToLimit(`Chat with ${applicationName ?? ''}`, NAME_MAX_LENGTH),
      is_private: true,
      source,
      participants: [applicationParticipant],
      chat_history: [],
      isNew: true,
      isApplicationChat: true,
    });
    setActiveParticipant(applicationParticipant);
    setHasRestoredConversation(false);
    setIsCreating(false);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [applicationParticipant, activeConversation, isCreating, applicationName, source, restoredConversationID]);

  return isCreating;
}

/** Keeps the conversation's `application`-typed participant entry in sync with `applicationParticipant` (e.g. after a version switch changes its `entitySettings`). */
function useSyncParticipantEffect(
  applicationParticipant: Participant | null,
  setActiveConversation: (update: (prev: ChatConversation | null) => ChatConversation | null) => void,
): void {
  useEffect(() => {
    if (!applicationParticipant) return;
    setActiveConversation((prev) => {
      if (!prev?.participants?.some((p) => p.entityName === 'application')) return prev;
      return {
        ...prev,
        participants: prev.participants.map((p) => (p.entityName === 'application' ? applicationParticipant : p)),
      };
    });
  }, [applicationParticipant, setActiveConversation]);
}

/** Keeps `chat_history[0]` in sync with the version's `welcome_message` (adding/removing/replacing it), skipped once a restored conversation has taken over. */
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

export function useApplicationChatConversation(
  params: UseApplicationChatConversationParams,
): UseApplicationChatConversationResult {
  const { applicationId, applicationName, applicationVersionDetails, projectId, source, restoredConversationID } = params;

  const [activeConversation, setActiveConversation] = useState<ChatConversation | null>(null);
  const [activeParticipant, setActiveParticipant] = useState<Participant | null>(null);
  const [hasRestoredConversation, setHasRestoredConversation] = useState(false);
  const chatHistoryRef = useRef<ChatHistoryMessage[]>([]);

  // Content-stable proxy for `applicationVersionDetails`: callers (e.g. a Formik-backed
  // parent that spreads `values.version_details` into a fresh object every render) may
  // hand this hook a NEW object identity each render even when nothing it actually reads
  // changed. Depending on the object itself in `applicationParticipant`'s memo below would
  // recompute — and therefore hand out a new `applicationParticipant` reference — on EVERY
  // render, which `useSyncParticipantEffect` (this file, below) treats as "the participant
  // changed" and calls `setActiveConversation`, which re-renders, which re-derives a new
  // `applicationVersionDetails`, forever: an infinite render loop (independently reproduced,
  // "Maximum update depth exceeded" at 302 renders). `JSON.stringify` of only the fields
  // `buildApplicationParticipant` actually reads is a primitive (string) that compares equal
  // by VALUE across renders when the content hasn't changed, so the outer `useMemo` skips
  // recomputation and `applicationParticipant` keeps its previous, referentially-stable value.
  const applicationVersionKey = useMemo(
    () =>
      applicationVersionDetails
        ? JSON.stringify({
            id: applicationVersionDetails.id,
            variables: applicationVersionDetails.variables,
            iconMeta: applicationVersionDetails.meta?.icon_meta ?? null,
            agentType: applicationVersionDetails.agent_type ?? null,
          })
        : undefined,
    [applicationVersionDetails],
  );

  // `applicationVersionKey` above is the intentional, content-stable substitute for
  // `applicationVersionDetails` (see its own comment); depending on the object directly
  // reintroduces the infinite-loop bug this guards against.
  /* eslint-disable react-hooks/exhaustive-deps */
  const applicationParticipant = useMemo(
    () => buildApplicationParticipant(applicationId, applicationName, applicationVersionDetails, projectId),
    [applicationId, applicationName, applicationVersionKey, projectId],
  );
  /* eslint-enable react-hooks/exhaustive-deps */

  const { onAttachFiles, attachments, onDeleteAttachment, disableAttachments, onClearAttachments } = useAgentAttachments({
    internalTools: applicationVersionDetails?.meta?.internal_tools,
    versionId: applicationVersionDetails?.id,
  });

  useRestoreConversationEffect(params, setActiveConversation, setActiveParticipant, chatHistoryRef, setHasRestoredConversation);
  const isCreatingConversation = useInitializeConversationEffect(
    applicationParticipant,
    activeConversation,
    applicationName,
    source,
    restoredConversationID,
    setActiveConversation,
    setActiveParticipant,
    setHasRestoredConversation,
  );
  useSyncParticipantEffect(applicationParticipant, setActiveConversation);

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
    applicationVersionDetails?.welcome_message,
    restoredConversationID,
    activeParticipant?.id,
    hasRestoredConversation,
    setChatHistory,
  );

  useEffect(() => {
    chatHistoryRef.current = activeConversation?.chat_history ?? [];
  }, [activeConversation?.chat_history]);

  // Resets to a fresh welcome-message-only history whenever the version id itself CHANGES —
  // baseline's own empty-deps unmount-of-old-version effect (`useApplicationChat.hooks.js`'s
  // final `useEffect`).
  //
  // **Bug fix (found writing this file's own tests, not a pre-existing test's regression):**
  // a plain `useEffect(fn, [applicationVersionDetails?.id])` runs on EVERY commit whose deps
  // changed, but ALSO unconditionally once on the initial mount — there is no "previous value"
  // to diff against yet, so React just runs it. That means this effect used to fire on mount
  // too, and since `prev?.chat_history.length` is checked (not `prev?.id`), a just-restored
  // conversation (real `id`/`uuid`, non-empty `chat_history` — restoring a conversation with NO
  // messages would be pointless, so this is the common case) had its `id`/`uuid` wiped back to
  // `undefined` the instant it was restored, on this same mount's effects flush — turning a
  // restored conversation back into an unsaved draft (breaking `onSend`'s "existing vs. needs
  // creation" branch, socket room joins keyed off the id, delete-message/delete-all, ...).
  // Reproduced directly by `useApplicationChatConversation.hooks.test.tsx`'s own
  // "adopts restoredConversationData once it resolves" test before this guard existed. A
  // `previousVersionIdRef` — seeded with the id BEFORE this effect's own first run, only
  // advanced (and only resets the conversation) once the id has genuinely changed from a prior
  // commit — restores the effect's intended "on version CHANGE, not on mount" semantics without
  // touching its own reset behaviour for a real subsequent version switch.
  const previousVersionIdRef = useRef(applicationVersionDetails?.id);
  useEffect(() => {
    if (previousVersionIdRef.current === applicationVersionDetails?.id) return;
    previousVersionIdRef.current = applicationVersionDetails?.id;
    setActiveConversation((prev) =>
      prev?.chat_history.length
        ? {
            ...prev,
            chat_history: getInitialChatHistory(applicationVersionDetails?.welcome_message, activeParticipant?.id ?? null),
            uuid: undefined,
            id: undefined,
          }
        : prev,
    );
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [applicationVersionDetails?.id]);

  return {
    activeConversation,
    activeParticipant,
    setActiveConversation,
    setActiveParticipant,
    isCreatingConversation,
    applicationParticipant,
    chatHistoryRef,
    setChatHistory,
    onAttachFiles,
    attachments,
    onDeleteAttachment,
    disableAttachments,
    onClearAttachments,
  };
}
