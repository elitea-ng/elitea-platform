/**
 * Local UI state for ChatBox.
 *
 * Manages the component's internal React state — mentions, conversation
 * starters, speaking mode, recommendation list visibility, and the "@"/"/"/
 * "~" symbol-processing state machines — that does not flow from the
 * conversation entity layer. The three symbol systems are the REAL ported
 * hooks (`chatInputCompositionHooks.useNewInputKeyDownHandler`,
 * `mentionHooks.useSlashMention`, `mentionHooks.useChatSkillMention` —
 * `features/chat-input`'s public barrel), composed here rather than
 * re-implemented, matching baseline's `combinedKeyDown`/`combinedInputChange`
 * (`ChatBox.jsx` lines ~1508-1608).
 *
 * Port of `ChatBox.jsx` local `useState` calls (lines ~280–430) plus the
 * "@"/"/"/"~" wiring (~1508-1608) and the broken/version-missing active-
 * participant guards (~2035-2051).
 */
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import type { KeyboardEvent, RefObject } from 'react';

import type { Participant } from '@/entities/participant';
import { isVersionNotFound } from '@/entities/version';
import type { VersionSummary } from '@/entities/version';
import { chatInputCompositionHooks, mentionHooks } from '@/features/chat-input';
import type { ChatInputHandle } from '@/features/chat-input';

/**
 * The substring a withdrawal stamps into the reverted clone's name. The
 * server's own marker — see `isActiveParticipantWithdrawn`.
 */
const WITHDRAWN_VERSION_MARKER = '-withdrawn-';

/** Mirrors old-app `common/constants:PUBLIC_PROJECT_ID` — not re-exported from `features/chat-participants`'s barrel (§3.5 cap), same env-var read that barrel's own `model/constants.ts` does. */
const PUBLIC_PROJECT_ID = (import.meta.env['VITE_PUBLIC_PROJECT_ID'] as string | undefined) || '0';

/* ------------------------------------------------------------------ */
/*  Shared shapes                                                       */
/* ------------------------------------------------------------------ */

/** A user participant resolved from conversation participants. Carries an index signature so it structurally satisfies `features/chat-input`'s `MentionCandidate` (`@`-detection's own candidate shape) without a mapping step. */
export interface ResolvedUserMention {
  readonly id: string;
  readonly name: string;
  readonly participant: unknown;
  readonly [key: string]: unknown;
}

/**
 * A conversation starter suggestion — the string the empty-chat grid renders.
 *
 * Declared as `{id, text}` until now, which was a LIE about every consumer.
 * The only thing this value ever reaches is `ChatConversationStarters`, whose
 * own prop is `readonly unknown[]` and which puts each entry through
 * `conversationStartersToStrings` before rendering it; nothing anywhere reads
 * `.id` or `.text`. An `{id, text}` row handed to it renders the literal
 * `[object Object]`, so the object form was not merely unused — it was the
 * one shape that could not work. The producers are plain strings already
 * (`ApplicationVersionDetail.conversationStarters`, and the agent editor's
 * RHF field), so this makes the declared type match both ends.
 */
export type ConversationStarter = string;

/* ------------------------------------------------------------------ */
/*  Hook                                                                */
/* ------------------------------------------------------------------ */

/**
 * Params for local state hook — only the values needed for initialisation
 * or reactive recalculations. Callbacks are provided as direct parameters
 * of the returned state fields, keeping the hook result flat.
 */
export interface UseChatBoxStateParams {
  /** Currently active participant (for recommendation list gating, skill mention, broken/version-missing guards). */
  readonly activeParticipant: Participant | undefined;
  /** Conversation participants (for user mention resolution and the "/" toolkit dropdown). */
  readonly participants: readonly Participant[] | undefined;
  /** Current user ID (for filtering own user from mentions). */
  readonly userId: string | undefined;
  /** Conversation starter items from props. */
  readonly conversationStarters: readonly ConversationStarter[] | undefined;
  /** Whether this is the agents page (gates certain state). */
  readonly isAgentsPage: boolean | undefined;
  /** Imperative handle of the chat textarea — the "/"/"~"/"@" systems replace text ranges through this. */
  readonly chatInput: RefObject<ChatInputHandle | null>;
  /** Fallback project id for the "~" skill dropdown's skills query. */
  readonly projectId: string | undefined;
  /** `activeParticipant`'s resolved version list (composition root's own `useActiveParticipantDetails` call) — drives the broken/version-missing guards below. */
  readonly activeParticipantVersions: readonly VersionSummary[] | undefined;
}

export interface UseChatBoxStateResult {
  /** Selected user mentions for "send to user" mode. */
  readonly selectedUsers: readonly ResolvedUserMention[];
  readonly setSelectedUsers: (users: readonly ResolvedUserMention[]) => void;
  /** Whether `@everyone` was mentioned. */
  readonly isMentioningEveryone: boolean;
  readonly setIsMentioningEveryone: (flag: boolean) => void;
  /** Whether a conversation starter has already been sent. */
  readonly hasStarterBeenSent: boolean;
  readonly setHasStarterBeenSent: (flag: boolean) => void;
  /** Speaking mode (hands-free voice input). */
  readonly isSpeakingMode: boolean;
  readonly setIsSpeakingMode: (flag: boolean) => void;
  /** Whether the recommendation (participant) list is visible. */
  readonly showRecommendationList: boolean;
  readonly setShowRecommendationList: (flag: boolean) => void;
  /** Whether conversation starters should be displayed. */
  readonly shouldShowStarters: boolean;
  /** The effective user mentions for the current conversation. */
  readonly users: readonly ResolvedUserMention[];
  /** Whether the conversation has users other than the current one. */
  readonly hasOtherUsers: boolean;
  /** baseline: `isActiveParticipantBroken` (`ChatBox.jsx:2035-2041`) — a public-project participant whose current version isn't in its own version list. */
  readonly isActiveParticipantBroken: boolean;
  /** #972 — the agent this conversation uses was published and has since been withdrawn. */
  readonly isActiveParticipantWithdrawn: boolean;
  /** baseline: `isActiveParticipantVersionMissing` (`ChatBox.jsx:2043-2050`). */
  readonly isActiveParticipantVersionMissing: boolean;
  /** The "@"/"#" trigger-detection state machine (`chatInputCompositionHooks.useNewInputKeyDownHandler`). */
  readonly keyDown: ReturnType<typeof chatInputCompositionHooks.useNewInputKeyDownHandler>;
  /** The "/" toolkit-tool mention state machine (`mentionHooks.useSlashMention`). */
  readonly slash: ReturnType<typeof mentionHooks.useSlashMention>;
  /** The "~" skill mention state machine (`mentionHooks.useChatSkillMention`). */
  readonly skill: ReturnType<typeof mentionHooks.useChatSkillMention>;
  /** `skill.skillPhase !== 'idle'`. */
  readonly isSkillPhaseActive: boolean;
  /** Combined highlight ranges from the "/" and "~" phases (baseline: `combinedHighlightRanges`). */
  readonly combinedHighlightRanges: readonly { start: number; end: number }[];
  /** Drives `keyDown`/`skill`/`slash` off one textarea keydown (baseline: `combinedKeyDown`). */
  readonly onNormalKeyDown: (event: KeyboardEvent<HTMLDivElement>) => void;
  /** Drives `slash`/`skill` off one textarea input-change (baseline: `combinedInputChange`). */
  readonly onInputChange: (value: string) => void;
  /** Commits a picked "@" user mention into the input text (baseline: `onSelectUserMention`). */
  readonly onSelectUserMention: (user: ResolvedUserMention) => void;
}

/** Closes participant recommendations once for each active-participant change. */
export function useResetRecommendationsOnParticipantChange(
  participantId: string | undefined,
  resetRecommendations: () => void,
): void {
  const previousParticipantId = useRef(participantId);
  useEffect(() => {
    if (previousParticipantId.current === participantId) return;
    resetRecommendations();
    previousParticipantId.current = participantId;
  }, [participantId, resetRecommendations]);
}

/**
 * Hook that manages all local UI state for the ChatBox composition root.
 * This replaces the ~15 individual `useState` calls in the old ChatBox.jsx
 * with a single, cohesive state bundle that can be easily reasoned about.
 */
export function useChatBoxState(params: UseChatBoxStateParams): UseChatBoxStateResult {
  const { activeParticipant, participants, userId, conversationStarters, isAgentsPage, chatInput, projectId, activeParticipantVersions } = params;

  // -- Local state --
  const [selectedUsers, setSelectedUsers] = useState<readonly ResolvedUserMention[]>([]);
  const [isMentioningEveryone, setIsMentioningEveryone] = useState(false);
  const [hasStarterBeenSent, setHasStarterBeenSent] = useState(false);
  const [isSpeakingMode, setIsSpeakingMode] = useState(false);
  const [showRecommendationList, setShowRecommendationList] = useState(false);

  // -- Computed: user mentions --
  const users = useMemo<ResolvedUserMention[]>(() => {
    const result: ResolvedUserMention[] = [];
    for (const p of participants ?? []) {
      const metaUserName = p.meta?.userName;
      if (p.entityName === 'user' && p.entityMeta?.id && metaUserName && p.entityMeta.id !== userId) {
        result.push({ id: p.id, name: metaUserName, participant: p });
      }
    }
    result.push({ id: '@everyone', name: 'Everyone', participant: 'All users' });
    return result;
  }, [participants, userId]);

  const hasOtherUsers = useMemo(
    () => (participants ?? []).some((p) => p.entityName === 'user' && p.entityMeta?.id && p.entityMeta.id !== userId),
    [participants, userId],
  );

  // -- "@"/"#" trigger detection, "/" toolkit mention, "~" skill mention --
  const keyDown = chatInputCompositionHooks.useNewInputKeyDownHandler({ disableHashtagDetection: !!isAgentsPage });
  const slash = mentionHooks.useSlashMention({ chatInput, participants });
  const skill = mentionHooks.useChatSkillMention({
    chatInput,
    activeParticipant: activeParticipant ?? null,
    ...(projectId !== undefined ? { projectId } : {}),
  });

  const isSkillPhaseActive = skill.skillPhase !== 'idle';

  const combinedHighlightRanges = useMemo(
    () => [...slash.highlightRanges, ...skill.skillHighlightRanges].sort((a, b) => a.start - b.start),
    [slash.highlightRanges, skill.skillHighlightRanges],
  );

  const onNormalKeyDown = useCallback(
    (event: KeyboardEvent<HTMLDivElement>) => {
      keyDown.onKeyDown(event);
      if (isSkillPhaseActive) {
        skill.onSkillKeyDown(event);
        return;
      }
      slash.onKeyDown(event);
    },
    [keyDown, isSkillPhaseActive, skill, slash],
  );

  const onInputChange = useCallback(
    (value: string) => {
      slash.onInputChange(value);
      skill.onSkillInputChange(value);
    },
    [slash, skill],
  );

  const onSelectUserMention = useCallback(
    (user: ResolvedUserMention) => {
      const anchor = keyDown.atAnchorRef.current;
      if (chatInput.current && anchor !== null) {
        chatInput.current.replaceRange(anchor, anchor + keyDown.atQuery.length, `@${user.name} `);
      }
      keyDown.stopProcessingAtSymbol();
    },
    [chatInput, keyDown],
  );

  // -- Computed: should show starters --
  const shouldShowStarters = useMemo(
    () => !keyDown.isProcessingSymbols && (conversationStarters?.length ?? 0) > 0,
    [keyDown.isProcessingSymbols, conversationStarters],
  );

  // -- Broken / version-missing active-participant guards (baseline: `ChatBox.jsx:2035-2051`) --
  const isActiveParticipantBroken = useMemo(() => {
    if (!activeParticipant) return false;
    if (activeParticipant.entityMeta?.projectId !== PUBLIC_PROJECT_ID) return false;
    if (!activeParticipantVersions) return false;
    const versionId = activeParticipant.entitySettings?.versionId;
    return !activeParticipantVersions.some((v) => v.id === String(versionId));
  }, [activeParticipant, activeParticipantVersions]);

  /**
   * The conversation's agent was PUBLISHED and has since been WITHDRAWN
   * (#972).
   *
   * Nothing in the chat surface used to read a participant's published state
   * at all, so a conversation whose agent left the catalogue looked entirely
   * live: no notice, an editable composer, an enabled Send. The holder could
   * only find out by sending.
   *
   * THE SIGNAL, and why it is this one. A withdrawal does not delete the
   * published clone — it REVERTS it to a draft and RENAMES it, stamping
   * `-withdrawn-` into the name (the server's own marker, pinned by
   * `agents.publishing.spec.ts`'s "after a withdrawal the same version name is
   * free again"). So the bound version still resolves, which is why
   * `isActiveParticipantBroken` above — written for a version that is GONE —
   * never fires for this case.
   *
   * Status alone is not enough: an ordinary private agent's version is a draft
   * too, and treating every draft as withdrawn would put this notice on most
   * conversations in the product. Both halves together are specific to a
   * version that WAS published and no longer is.
   *
   * ── WHY THE VERSION-LIST DERIVATION BELOW IS NOW THE FALLBACK ──────────
   *
   * That reading is correct about what the server writes and was, on the chat
   * page, UNREACHABLE. `activeParticipantVersions` comes from
   * `useActiveParticipantDetails`, which fires only once a participant has
   * been SELECTED — and opening a conversation selects nothing: the active
   * participant is restored from localStorage, so a conversation opened in a
   * fresh browser has none, and the guard short-circuited on an undefined
   * version list. For a participant bound into the PUBLIC project it could not
   * be made to work at all: the public-application read serves `version_details`
   * and no `versions` array.
   *
   * So the SERVER now answers it, on the participant itself
   * (`meta.version_withdrawn`, written by
   * `repos.ConversationsRepo.enrichAgentParticipantPublication`). That makes
   * the state a property of the CONVERSATION, which is what it is, rather than
   * of a selection the reader has not made — which is why the server flag is
   * read across the conversation's agent participants when none is active.
   *
   * The version-list derivation is kept as the fallback for a server that has
   * not been rebuilt yet, and only for the participant that IS active, where
   * it was already proven correct.
   */
  const isActiveParticipantWithdrawn = useMemo(() => {
    // THE SERVER FIRST. `undefined` is "not resolved" and must fall through;
    // only an explicit boolean settles it.
    const agents = activeParticipant
      ? [activeParticipant]
      : (participants ?? []).filter((candidate) => candidate.entityName === 'application');
    const resolved = agents.filter((agent) => typeof agent.meta?.versionWithdrawn === 'boolean');
    if (resolved.length > 0) return resolved.some((agent) => agent.meta?.versionWithdrawn === true);

    if (!activeParticipant || !activeParticipantVersions?.length) return false;
    const versionId = activeParticipant.entitySettings?.versionId;
    if (versionId === undefined) return false;
    const bound = activeParticipantVersions.find((version) => version.id === String(versionId));
    if (bound === undefined) return false;
    return bound.status !== 'published' && bound.name.includes(WITHDRAWN_VERSION_MARKER);
  }, [activeParticipant, activeParticipantVersions, participants]);

  const isActiveParticipantVersionMissing = useMemo(() => {
    if (!activeParticipant) return false;
    if (!activeParticipantVersions?.length) return false;
    const versionId = activeParticipant.entitySettings?.versionId;
    if (versionId === undefined) return false;
    return isVersionNotFound(String(versionId), activeParticipantVersions);
  }, [activeParticipant, activeParticipantVersions]);

  // -- Gate recommendation list when active participant changes --
  // (The composition root controls this via `activeParticipant` param.
  //  We reset the recommendation list whenever the participant changes.)
  const resetRecommendations = useCallback(() => setShowRecommendationList(false), []);
  useResetRecommendationsOnParticipantChange(activeParticipant?.id, resetRecommendations);

  return {
    selectedUsers,
    setSelectedUsers,
    isMentioningEveryone,
    setIsMentioningEveryone,
    hasStarterBeenSent,
    setHasStarterBeenSent,
    isSpeakingMode,
    setIsSpeakingMode,
    showRecommendationList,
    setShowRecommendationList,
    shouldShowStarters,
    users,
    hasOtherUsers,
    isActiveParticipantBroken,
    isActiveParticipantWithdrawn,
    isActiveParticipantVersionMissing,
    keyDown,
    slash,
    skill,
    isSkillPhaseActive,
    combinedHighlightRanges,
    onNormalKeyDown,
    onInputChange,
    onSelectUserMention,
  };
}
