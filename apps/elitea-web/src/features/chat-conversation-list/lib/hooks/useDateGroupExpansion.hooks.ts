import { useCallback, useRef, useState } from 'react';

import { conversationMatchId, DATE_GROUP_ORDER, DEFAULT_EXPANDED_GROUP } from '@/entities/folder';
import type { DateGroup } from '@/entities/folder';

export interface UseDateGroupExpansionResult {
  readonly isGroupExpanded: (groupName: string) => boolean;
  readonly toggleGroup: (groupName: string) => void;
  readonly expandTodayGroup: () => void;
  /** `selectedConversationMatchId` is a pre-computed `conversationMatchId(...)` string (baseline: `genConversationId(activeConversation)`), not a bare conversation id — same parameter shape as `entities/folder`'s own `resolveInitialExpandedGroup`. */
  readonly initializeExpansion: (groups: readonly DateGroup[], selectedConversationMatchId: string | undefined) => void;
  readonly enterSearchMode: (groupsWithResults: readonly string[]) => void;
  readonly exitSearchMode: (activeConversationGroup?: string) => void;
  readonly isSearchMode: boolean;
}

/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/chat/conversation-list/lib/
 * hooks/useDateGroupExpansion.hooks.js` (unit C2) — pure local
 * expand/collapse + search-mode state, no API/network dependency at all
 * (per the brief). `DATE_GROUP_ORDER`/`DEFAULT_EXPANDED_GROUP`/
 * `conversationMatchId`/the `DateGroup` type are still imported from
 * `entities/folder` rather than redeclared here: they are pure,
 * already-landed constants/selectors (no `folderApi` call, no network),
 * and duplicating them would directly contradict the sibling
 * `conversationList.constants.ts`'s own instruction not to re-port them.
 *
 * `entities/folder/model/selectors.ts` also exports a
 * `resolveInitialExpandedGroup` selector that looks similar to
 * `initializeExpansion` below, but it is NOT reused here: it is a pure,
 * stateless "which ONE group should be expanded" function, whereas this
 * hook's `initializeExpansion` is a stateful, multi-branch state machine
 * that (a) ADDS to the existing `expandedGroups` Set rather than replacing
 * it in its first two branches, (b) REPLACES the whole Set only in its
 * fallback branch, and (c) runs that fallback branch at most once, ever
 * (`initialGroupSet` guard) — porting the baseline's own `useState`/
 * `useRef` semantics faithfully takes priority over reusing the selector.
 */
export function useDateGroupExpansion(): UseDateGroupExpansionResult {
  // Values that don't need to trigger a re-render — same as the baseline's own comment.
  const searchModeRef = useRef(false);
  const savedNormalExpansionRef = useRef<Set<string> | null>(null);

  const [expandedGroups, setExpandedGroups] = useState<Set<string>>(new Set());
  const [initialGroupSet, setInitialGroupSet] = useState(false);

  const toggleGroup = useCallback((groupName: string): void => {
    setExpandedGroups((prev) => {
      const next = new Set(prev);
      if (next.has(groupName)) next.delete(groupName);
      else next.add(groupName);
      return next;
    });
  }, []);

  const isGroupExpanded = useCallback((groupName: string): boolean => expandedGroups.has(groupName), [expandedGroups]);

  /**
   * Expands exactly the groups that hold a result.
   *
   * The SAVE of the pre-search expansion happens once, on the way in — that is
   * the baseline's own semantics and what `exitSearchMode` restores. Applying
   * the new set, however, must happen on every call: a search result arrives
   * after the query is typed, so the caller has to be able to say "these are
   * the groups that hold matches now". The previous version returned early
   * whenever it was already in search mode, so the FIRST answer decided the
   * expansion for the whole search — and a call made before any result had
   * arrived (which is what a listing that unmounts while it refetches
   * produces) left every group collapsed with the results inside them.
   */
  const enterSearchMode = useCallback(
    (groupsWithResults: readonly string[]): void => {
      if (!searchModeRef.current) {
        searchModeRef.current = true;
        savedNormalExpansionRef.current = new Set(expandedGroups);
      }
      setExpandedGroups(new Set(groupsWithResults));
    },
    [expandedGroups],
  );

  const exitSearchMode = useCallback((activeConversationGroup?: string): void => {
    if (!searchModeRef.current) return;
    searchModeRef.current = false;

    if (activeConversationGroup !== undefined) {
      setExpandedGroups(new Set([activeConversationGroup]));
    } else {
      const savedExpansion = savedNormalExpansionRef.current;
      if (savedExpansion !== null && savedExpansion.size > 0) setExpandedGroups(new Set(savedExpansion));
      else setExpandedGroups(new Set([DEFAULT_EXPANDED_GROUP]));
    }

    savedNormalExpansionRef.current = null;
  }, []);

  const initializeExpansion = useCallback(
    (groups: readonly DateGroup[], selectedConversationMatchId: string | undefined): void => {
      if (groups.length === 0 || searchModeRef.current) return;

      const groupContainingSelected = groups.find((group) =>
        group.conversations.some((conversation) => conversationMatchId(conversation) === selectedConversationMatchId),
      );

      if (groupContainingSelected !== undefined) {
        setExpandedGroups((prev) => new Set([...prev, groupContainingSelected.name]));
        return;
      }

      const todayGroup = groups.find((group) => group.name === DEFAULT_EXPANDED_GROUP);
      if (todayGroup !== undefined) {
        setExpandedGroups((prev) => new Set([...prev, DEFAULT_EXPANDED_GROUP]));
        return;
      }

      if (!initialGroupSet) {
        for (const groupName of DATE_GROUP_ORDER) {
          const group = groups.find((g) => g.name === groupName);
          if (group !== undefined) {
            setExpandedGroups(new Set([groupName]));
            setInitialGroupSet(true);
            break;
          }
        }
      }
    },
    [initialGroupSet],
  );

  const expandTodayGroup = useCallback((): void => {
    setExpandedGroups((prev) => new Set([...prev, DEFAULT_EXPANDED_GROUP]));
  }, []);

  return {
    isGroupExpanded,
    toggleGroup,
    expandTodayGroup,
    initializeExpansion,
    enterSearchMode,
    exitSearchMode,
    isSearchMode: searchModeRef.current,
  };
}
