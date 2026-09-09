import type { ReactNode } from 'react';
import { useCallback, useEffect, useRef, useState } from 'react';

import type { Conversation } from '@/entities/conversation';
import { genConversationId } from '@/shared/lib/chat';
import type { ControlsDropdownLeafItem } from '@/shared/ui/ControlsDropdown';

import { useLatestRef } from '../../lib/hooks/useLatestRef';
import type { RenderConversationItem } from '../groups/DateGroup';
import { ConversationItem } from './ConversationItem';
import type { ConversationExportFormat } from './ConversationItem.menu';
import { renderFoldersSectionImpl } from './Conversations.folders';
import type { RenderFoldersSectionParams } from './Conversations.folders';
import type { ConversationsFolder } from './Conversations.types';

/**
 * `Conversations.tsx`'s two render-prop factories (`renderConversationItem`/
 * `renderFoldersSection`), split into their own file purely to keep
 * `Conversations.tsx` under the §3.5 `max-lines` budget (400) — each one's
 * own `useCallback` dependency list independently breached the SEPARATE
 * `hook-deps` budget (8) at 18/20 entries before this split, which is what
 * motivated bundling every input into one `useLatestRef` (see that hook's
 * own doc comment) rather than just moving the code verbatim.
 *
 * Both hooks below are safe to read `.current` synchronously (never behind
 * a later event) — `renderConversationItem`/the function `useRenderFoldersSection`
 * returns are only ever invoked as render-props by `ConversationsBody`/
 * `Conversations.folders.tsx`'s own `renderFoldersSectionImpl`, DURING the
 * same synchronous render pass that produced the ref assignment (React
 * finishes a function component's own body — including this ref write —
 * before recursing into any child it handed the function to).
 */
export interface UseRenderConversationItemParams {
  readonly selectedConversationId: string | undefined;
  readonly onSelectConversation: (conversation: Conversation) => void;
  readonly onEditConversation: (conversation: Conversation) => void;
  readonly onPlaybackConversation: (conversation: Conversation) => void;
  readonly onDeleteConversation: (conversation: Conversation) => void;
  readonly onPinConversation: (conversation: Conversation, shouldPin: boolean) => void;
  readonly onCreateConversation: (conversation: Conversation) => Promise<unknown>;
  readonly onCancelCreateConversation: () => void;
  readonly onChangeActiveConversationName: (name: string) => void;
  readonly getMoveConversationToFoldersMenuItems: (conversation: Conversation) => readonly ControlsDropdownLeafItem[];
  readonly isEditingCanvas: boolean;
  readonly enableDragAndDrop: boolean;
  readonly projectId: string | undefined;
  readonly currentUserId: string | undefined;
  readonly personalProjectId: string | number | undefined;
  readonly publicProjectId: string | number | undefined;
  /** `string | undefined`, not defaulted here — `Conversations.tsx` passes its own `basename` prop straight through, same as the pre-extraction code did; `ConversationItem`'s own `resolveConversationItemDefaults` applies the `?? ''` fallback. */
  readonly basename: string | undefined;
  readonly onShareLinkCopied: (() => void) | undefined;
  /**
   * Downloads one conversation's transcript (issue 851). Threaded through
   * this factory rather than resolved in the row, because the exporter needs
   * the project id and the toast that only the composition root holds.
   */
  readonly onExportConversation: (conversation: Conversation, format: ConversationExportFormat) => void;
}

export function useRenderConversationItem(params: UseRenderConversationItemParams): RenderConversationItem {
  const ref = useLatestRef(params);

  return useCallback((conversation, onItemHover, isNextItemHovered) => {
    const {
      selectedConversationId,
      onSelectConversation,
      onEditConversation,
      onPlaybackConversation,
      onDeleteConversation,
      onPinConversation,
      onCreateConversation,
      onCancelCreateConversation,
      onChangeActiveConversationName,
      getMoveConversationToFoldersMenuItems,
      isEditingCanvas,
      enableDragAndDrop,
      projectId,
      currentUserId,
      personalProjectId,
      publicProjectId,
      basename,
      onShareLinkCopied,
      onExportConversation,
    } = ref.current;

    return (
      <ConversationItem
        key={genConversationId(conversation)}
        isActive={selectedConversationId === genConversationId(conversation)}
        conversation={conversation}
        onSelectConversation={onSelectConversation}
        onEdit={onEditConversation}
        onPlayback={onPlaybackConversation}
        onDelete={onDeleteConversation}
        onPin={onPinConversation}
        onCreateConversation={onCreateConversation}
        onCancelCreate={onCancelCreateConversation}
        onChangeActiveConversationName={onChangeActiveConversationName}
        moveToFoldersMenuItems={getMoveConversationToFoldersMenuItems(conversation)}
        isEditingCanvas={isEditingCanvas}
        enableDragAndDrop={enableDragAndDrop}
        isDragDisabled={isEditingCanvas || conversation.isPlayback === true || conversation.isPinned === true}
        onItemHover={onItemHover}
        isNextItemHovered={isNextItemHovered}
        projectId={projectId}
        currentUserId={currentUserId}
        personalProjectId={personalProjectId}
        publicProjectId={publicProjectId}
        basename={basename}
        onShareLinkCopied={onShareLinkCopied}
        onExport={(format) => onExportConversation(conversation, format)}
      />
    );
    // eslint-disable-next-line react-hooks/exhaustive-deps -- every `ref.current.*` read above is intentionally NOT listed, per this file's own module doc (`useLatestRef`'s contract: `.current` is always this render's values by the time a render-prop like this one is invoked) — listing them would recreate this function on every one of their changes, exactly what the §3.5 `hook-deps` budget flagged as excessive.
  }, []);
}

/** Everything `renderFoldersSectionImpl` (`Conversations.folders.tsx`) needs EXCEPT the 2 per-call arguments (`sectionFolders`/`isPinnedSection`) `useRenderFoldersSection`'s returned function takes instead, and `forceRerenderKey`, which this hook computes itself (see below). */
export type UseRenderFoldersSectionParams = Omit<RenderFoldersSectionParams, 'sectionFolders' | 'isPinnedSection' | 'forceRerenderKey'>;

export function useRenderFoldersSection(
  params: UseRenderFoldersSectionParams,
): (sectionFolders: readonly ConversationsFolder[], isPinnedSection: boolean) => ReactNode {
  const ref = useLatestRef(params);

  /**
   * Baseline `Folders.jsx:140-152` — a full remount is the only way to reset
   * a folder's expansion back to its non-search default when leaving search
   * mode, since `FolderAccordion`'s own `expanded` state only ever syncs
   * FROM `defaultExpanded` going true (see that component's own doc
   * comment: a caller flipping `defaultExpanded` back to `false` does NOT
   * re-collapse an already-expanded accordion). Found missing from this
   * file by adversarial verify — `Conversations.folders.tsx`'s
   * `buildFolderItem` used a bare `key={folder.id}`, so a folder that
   * `computeFolderActivity` auto-expanded during a search (its
   * `isSearchMode && folder.hasSearchMatches` branch) stayed visibly
   * expanded forever after the search was cleared.
   */
  const [forceRerenderKey, setForceRerenderKey] = useState(0);
  const wasInSearchModeRef = useRef(params.isSearchMode);
  useEffect(() => {
    if (wasInSearchModeRef.current && !params.isSearchMode) setForceRerenderKey((prev) => prev + 1);
    wasInSearchModeRef.current = params.isSearchMode;
  }, [params.isSearchMode]);

  return useCallback(
    (sectionFolders, isPinnedSection) => renderFoldersSectionImpl({ ...ref.current, sectionFolders, isPinnedSection, forceRerenderKey }),
    // eslint-disable-next-line react-hooks/exhaustive-deps -- `ref` (a `useLatestRef` object) is intentionally NOT listed: its own identity never changes across renders (see `useLatestRef`'s doc comment — one `useRef` call, reassigned in place), so listing it would add nothing; reading `ref.current` inside is what stays fresh, by design. `forceRerenderKey` IS listed: unlike everything in `ref`, a change to it must actually produce a new callback identity so the new key value reaches `buildFolderItem` this same render.
    [forceRerenderKey],
  );
}
