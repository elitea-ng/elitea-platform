/**
 * Ported from
 * `apps/elitea-ui/src/[fsd]/features/chat/lib/hooks/useSlashMention.hooks.js`
 * — the higher-level hook combining "/" slash-mention state management with
 * chat-input text manipulation. Baseline doc comment: "Shared by
 * NewConversationView and ChatBox" — this port's real callers are a future
 * C4 (message-editing) and C6 (chat composition root) unit.
 *
 * **Signature deviation, forced by this app's entity model (disclosed).**
 * The baseline takes `activeConversation` and reads
 * `activeConversation.participants` (full baseline participant rows —
 * `entity_name`/`entity_settings`/`entity_meta`/`meta`). This app's
 * `entities/conversation`'s `Conversation.participants` is deliberately
 * THIN (`ConversationParticipantRef`: just `{id, entityName}` — see that
 * type's own doc comment) — the rich per-participant shape lives in
 * `entities/participant`'s `Participant` type instead. So this port takes
 * `participants: readonly Participant[] | undefined` directly rather than
 * an `activeConversation` object — the composition-root caller already has
 * this array from wherever it fetches conversation detail (the same array
 * a future participants-list feature would render), before it gets
 * summarised down to `Conversation.participants`' thin ref shape.
 *
 * **`useIsMcpVisible()` called internally** (not injected), matching the
 * baseline's own `useSlashMention.hooks.js:48` — see `./useIsMcpVisible.ts`
 * for why this is a 4th feature-local duplicate rather than a shared import.
 *
 * **Icon resolution — disclosed, established scope reduction.** The
 * baseline resolves a per-toolkit-brand icon via `getToolIconByType(type,
 * theme, {toolSchema, isMCP})` (`common/toolkitUtils.jsx`, ~30 brand SVGs)
 * when `entity_settings.icon_meta.url` is absent. This exact gap is already
 * disclosed three times in this app (`features/toolkits/ui/EntityIcon.tsx`,
 * `features/pipelines/ui/select/EntityOptionIcon.tsx`,
 * `features/agents/ui/generate-agent-modal/SuggestionItem.tsx`) — no port
 * of `getToolIconByType` exists anywhere. This port reads only
 * `entitySettings.iconMeta`'s `url` field (if present, as an `unknown`
 * blob) and otherwise leaves `iconUrl` undefined — `ui/ToolkitMentionList
 * .tsx`'s row renderer falls back to a generic toolkit/MCP glyph, same
 * "drop the decorative fanciness, keep the function" call those three
 * files already made.
 *
 * **`settings` deliberately NOT carried on `SlashParticipantToolkit`** —
 * verified against the baseline itself: `useSlashMention.hooks.js`'s own
 * `participantToolkits` mapping (`participants.filter(...).map(p => ({id,
 * project_id, type, name, icon_meta, participantType}))`) never included a
 * `settings` field either. The toolkit's `available_mcp_tools`/
 * `selected_tools` (needed for the 'tool' phase's list) come exclusively
 * from a fresh per-toolkit details fetch in the baseline too (`SlashSuggestionList
 * .jsx`'s `useToolkitsDetailsQuery`) — see `ui/SlashSuggestionList.tsx`'s
 * own doc comment for the injected-dependency treatment that endpoint gets
 * here (no generated client exists for it).
 */
import { useCallback, useMemo, useState } from 'react';
import type { KeyboardEvent, RefObject } from 'react';

import type { Participant } from '@/entities/participant';

import type { MentionRange } from '../utils/instructionsMention.utils';
import type { ChatInputHandle } from '../chatInputHandle';

import { useIsMcpVisible } from './useIsMcpVisible';
import { useSlashCommandHandler } from './useSlashCommandHandler';
import { useSlashHighlights } from './useSlashHighlights';
import type { CommittedToolkitMention, SlashPhase, SlashToolkitRef } from './useSlashCommandHandler.types';

/** One toolkit participant of the active conversation, as surfaced to the "/" toolkit dropdown — `useSlashMention.hooks.js`'s own `participantToolkits` row shape (see this file's header for why `settings` is not carried). */
export interface SlashParticipantToolkit {
  readonly id: string;
  readonly projectId: string;
  readonly type: string;
  readonly name: string;
  /** From `entitySettings.iconMeta`'s `url` field, when present — see this file's header for the disclosed per-brand-icon scope reduction. */
  readonly iconUrl?: string | undefined;
}

function extractIconUrl(iconMeta: unknown): string | undefined {
  if (typeof iconMeta !== 'object' || iconMeta === null) return undefined;
  const url = (iconMeta as Record<string, unknown>)['url'];
  return typeof url === 'string' ? url : undefined;
}

/** `mcp.helpers.js:7-14`'s `isMcpToolkitType` — `type === 'mcp'` or a `mcp_*` pre-built type. Duplicated locally (see `./useIsMcpVisible.ts`'s header for why no shared home exists) rather than reused from `entities/toolkit`'s `isMcpToolkit`, which takes a full `Toolkit` object this hook does not have. */
function isMcpToolkitType(type: string): boolean {
  return type === 'mcp' || type.startsWith('mcp_');
}

function toSlashParticipantToolkit(participant: Participant): SlashParticipantToolkit | null {
  if (participant.entityName !== 'toolkit') return null;
  const id = participant.entityMeta?.id;
  const projectId = participant.entityMeta?.projectId;
  if (id === undefined || projectId === undefined) return null;
  const iconUrl = extractIconUrl(participant.entitySettings?.iconMeta);
  return {
    id,
    projectId,
    type: participant.entitySettings?.toolkitType ?? '',
    name: participant.meta?.name ?? '',
    ...(iconUrl !== undefined ? { iconUrl } : {}),
  };
}

export interface UseSlashMentionParams {
  /** Imperative handle of whatever chat-textarea component renders the actual input — see `../chatInputHandle.ts`. */
  readonly chatInput: RefObject<ChatInputHandle | null>;
  /** The active conversation's full participant list — see this file's header for why this replaces the baseline's `activeConversation`. `undefined` (conversation not loaded yet) behaves the same as `[]`. */
  readonly participants: readonly Participant[] | undefined;
}

export interface UseSlashMentionResult {
  readonly phase: SlashPhase;
  readonly toolkitQuery: string;
  readonly toolQuery: string;
  readonly selectedToolkit: SlashToolkitRef | null;
  readonly committedMentions: readonly CommittedToolkitMention[];
  readonly isQueryFinal: boolean;
  readonly onKeyDown: (event: KeyboardEvent) => void;
  /** Toolkit participants of the active conversation, filtered by MCP visibility (AC1-adjacent — matches the baseline's own gate). */
  readonly participantToolkits: readonly SlashParticipantToolkit[];
  readonly isMcpVisible: boolean;
  readonly resetSlash: () => void;
  readonly clearMentions: () => void;
  /** Replaces the typed "/query" (or "/query/") fragment with "/toolkit.name" in the input, then advances to 'tool' phase. */
  readonly onSelectToolkit: (toolkit: SlashParticipantToolkit) => void;
  /** Replaces the "/toolkit[/toolQuery]" fragment with the final mention token (`toolName: null \| undefined` commits the whole toolkit). */
  readonly onCommitMention: (toolName?: string | null) => void;
  readonly onInputChange: (value: string) => void;
  /** The `Skills` entry of the list: writes `/skill ` over the typed "/" fragment (`~` when it is not at the start of a line, where `/skill` would not open the picker), closes the list, and returns the new text and the cursor after the inserted token (the textarea moves its own cursor a tick later), so the skill picker can open on it; `null` when there is no input. */
  readonly onSelectSkillsEntry: () => { readonly text: string; readonly cursor: number } | null;
  /** Character ranges within the current input text that correspond to committed mention tokens — for the caller's highlight backdrop. */
  readonly highlightRanges: readonly MentionRange[];
  readonly activeIndex: number;
  readonly setActiveIndex: (index: number) => void;
  readonly itemCountRef: { current: number };
  readonly onConfirmActiveRef: { current: ((index: number) => void) | null };
}

export function useSlashMention({ chatInput, participants }: UseSlashMentionParams): UseSlashMentionResult {
  // Track a copy of the current input text so highlight ranges can be computed
  // without reading the DOM imperative ref on every render.
  const [inputContent, setInputContent] = useState('');
  const isMcpVisible = useIsMcpVisible();

  const {
    phase,
    toolkitQuery,
    toolQuery,
    selectedToolkit,
    committedMentions,
    isQueryFinal,
    onKeyDown,
    syncWithValue,
    selectToolkit,
    commitMention,
    clearMentions,
    resetSlash,
    mentionAnchorRef,
    activeIndex,
    setActiveIndex,
    itemCountRef,
    onConfirmActiveRef,
  } = useSlashCommandHandler({ setInputContent });

  // IDs of toolkit participants in the current conversation, used by SlashSuggestionList
  // to filter the autosuggest to only those already added (AC1).
  const participantToolkits = useMemo<readonly SlashParticipantToolkit[]>(() => {
    if (!participants?.length) return [];
    const result: SlashParticipantToolkit[] = [];
    for (const participant of participants) {
      const toolkit = toSlashParticipantToolkit(participant);
      if (!toolkit) continue;
      if (!isMcpVisible && isMcpToolkitType(toolkit.type)) continue;
      result.push(toolkit);
    }
    return result;
  }, [participants, isMcpVisible]);

  // Replace the typed "/query" or "/query/" fragment with "/toolkit.name" in the input.
  // Uses mentionAnchorRef so that editing an earlier mention in the middle of the text
  // replaces only the correct fragment (not the last mention in the string).
  const onSelectToolkit = useCallback(
    (toolkit: SlashParticipantToolkit) => {
      const ref = chatInput.current;
      if (ref) {
        const content = ref.getInputContent();
        const anchor = mentionAnchorRef.current ?? content.length;
        // Use cursor position as the fragment end instead of whitespace search.
        // Whitespace search breaks for toolkit names that contain spaces — it stops
        // at the first space inside the name rather than at the end of the typed fragment.
        const cursorPos = ref.getCursorPosition() ?? content.length;
        // Fragment is text from anchor up to (but not including) the cursor.
        const afterAnchor = content.slice(anchor, cursorPos);
        // When the fragment already contains the separator '/' (e.g. "/typedname/"),
        // include it in both the replaced range and the replacement so that:
        //   • the separator is preserved in the text, and
        //   • replaceRange places the cursor AFTER the separator (not before it).
        const separatorIdx = afterAnchor.indexOf('/', 1); // skip the leading '/'
        const hasSeparatorInFragment = separatorIdx !== -1;
        // Also check if the separator sits immediately at the cursor (cursor between
        // toolkit name and tool query, e.g. "/name|/toolQuery").
        const hasSeparatorAtCursor = !hasSeparatorInFragment && content[cursorPos] === '/';
        const hasSeparator = hasSeparatorInFragment || hasSeparatorAtCursor;

        let replaceEnd = cursorPos;
        if (hasSeparatorInFragment) replaceEnd = anchor + separatorIdx + 1;
        else if (hasSeparatorAtCursor) replaceEnd = cursorPos + 1;

        const replacement = hasSeparator ? '/' + toolkit.name + '/' : '/' + toolkit.name;
        ref.replaceRange(anchor, replaceEnd, replacement);
        // replaceRange bypasses onChange, so sync local inputContent manually so that
        // useSlashHighlights can compute highlight ranges without waiting for a keystroke.
        setInputContent(content.slice(0, anchor) + replacement + content.slice(replaceEnd));
      }
      selectToolkit({ id: toolkit.id, projectId: toolkit.projectId, name: toolkit.name, type: toolkit.type });
    },
    [chatInput, mentionAnchorRef, selectToolkit],
  );

  // Replace the "/toolkit[/toolQuery]" fragment with the final mention token.
  // Uses mentionAnchorRef for precise bounds so that committing an earlier mention
  // never overwrites text that follows it.
  const onCommitMention = useCallback(
    (toolName?: string | null) => {
      const ref = chatInput.current;
      if (ref && selectedToolkit) {
        const content = ref.getInputContent();
        const anchor = mentionAnchorRef.current ?? 0;
        // Locate mention end using the toolkit name directly so that toolkit names
        // containing spaces are handled correctly (whitespace search would truncate early).
        const toolkitPrefix = '/' + selectedToolkit.name + '/';
        let mentionEnd: number;
        if (content.startsWith(toolkitPrefix, anchor)) {
          // /toolkitName/ is present — advance past the separator then skip the tool query.
          const afterPrefix = content.slice(anchor + toolkitPrefix.length);
          const endIdx = afterPrefix.search(/[\s/]/);
          mentionEnd = anchor + toolkitPrefix.length + (endIdx === -1 ? afterPrefix.length : endIdx);
        } else {
          // No separator yet — mention ends right after the toolkit name.
          mentionEnd = anchor + ('/' + selectedToolkit.name).length;
        }
        const mentionToken = toolName ? `/${selectedToolkit.name}/${toolName}` : `/${selectedToolkit.name}`;
        const replacement = mentionToken + ' ';
        ref.replaceRange(anchor, mentionEnd, replacement);
        // replaceRange bypasses onChange; sync local inputContent so the highlight
        // mirror stays aligned immediately (before the next keystroke fires onChange).
        setInputContent(content.slice(0, anchor) + replacement + content.slice(mentionEnd));
      }
      commitMention(toolName);
    },
    [chatInput, mentionAnchorRef, selectedToolkit, commitMention],
  );

  const onInputChange = useCallback(
    (value: string) => {
      setInputContent(value);
      if (!value) {
        clearMentions();
        resetSlash();
        return;
      }
      const cursorPos = chatInput.current?.getCursorPosition() ?? null;
      syncWithValue(value, cursorPos);
    },
    [chatInput, syncWithValue, clearMentions, resetSlash],
  );

  const onSelectSkillsEntry = useCallback((): { readonly text: string; readonly cursor: number } | null => {
    const ref = chatInput.current;
    if (!ref) return null;
    const content = ref.getInputContent();
    const anchor = mentionAnchorRef.current;
    if (anchor === null) {
      resetSlash();
      return null;
    }
    const cursorPos = Math.max(anchor, ref.getCursorPosition() ?? content.length);
    const atLineStart = anchor === 0 || content[anchor - 1] === '\n';
    const replacement = atLineStart ? '/skill ' : '~';
    ref.replaceRange(anchor, cursorPos, replacement);
    const next = content.slice(0, anchor) + replacement + content.slice(cursorPos);
    setInputContent(next);
    resetSlash();
    return { text: next, cursor: anchor + replacement.length };
  }, [chatInput, mentionAnchorRef, resetSlash]);

  // Character ranges within inputContent that correspond to committed mention tokens —
  // for the caller's highlight backdrop.
  const highlightRanges = useSlashHighlights(inputContent, committedMentions);

  return {
    phase,
    toolkitQuery,
    toolQuery,
    selectedToolkit,
    committedMentions,
    isQueryFinal,
    onKeyDown,
    participantToolkits,
    isMcpVisible,
    resetSlash,
    clearMentions,
    onSelectToolkit,
    onCommitMention,
    onInputChange,
    onSelectSkillsEntry,
    highlightRanges,
    activeIndex,
    setActiveIndex,
    itemCountRef,
    onConfirmActiveRef,
  };
}
