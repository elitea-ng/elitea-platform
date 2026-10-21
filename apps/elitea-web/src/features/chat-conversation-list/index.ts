/**
 * Public API — spec §3.3: named exports only, curated (§3.5 budget: ≤20
 * exported symbols, enforced by `scripts/check-budgets.mjs`).
 *
 * Unlike `features/agents`/`features/pipelines` (this same worktree), this
 * slice's whole `lib/`+`ui/` tree landed in ONE unit (C2) — this budget is
 * not shared with any sibling sub-unit, but 16/20 is still spent
 * deliberately rather than maxed out: only symbols a future composition-
 * root page (baseline: `pages/NewChat/NewChat.jsx`) genuinely needs to wire
 * this feature together are listed below.
 *
 * `Conversations` (`./ui/conversations/Conversations.tsx`) is the
 * composition root other units will render — its own ~35-prop
 * `ConversationsProps` is the load-bearing contract. Every OTHER component
 * under `ui/` (`ConversationItem`, `FolderItem`, `GroupedConversations`,
 * `DateGroup`, …) is an implementation detail `Conversations.tsx` itself
 * composes — not exported, matching how `entities/conversation` doesn't
 * re-export its own internal helpers and how `features/agents/index.ts`'s
 * own doc comment draws the identical line for its 12 intra-slice-only
 * sub-components. (`ui/folders/Folders.tsx`, an earlier full port of the
 * baseline's own `Folders.jsx` wrapper, was removed by the same
 * adversarial-verify pass that found this: `Conversations.tsx`'s own
 * folder-section rendering — `Conversations.folders.tsx`'s
 * `renderFoldersSectionImpl`, called from `Conversations.renderers.tsx`'s
 * `useRenderFoldersSection` — was built independently and is what's
 * actually wired in; `Folders.tsx` had zero real callers. Its one genuine
 * behavioral edge over the wired path — the search-mode-exit
 * `forceRerenderKey` remount, needed because `FolderAccordion`'s own
 * `expanded` state only ever syncs FROM `defaultExpanded` going true — has
 * been ported into `useRenderFoldersSection` itself, so no behavior was
 * lost by the removal.)
 *
 * 9 of the directory's 13 `lib/hooks/*` files are exported as the
 * independently-callable state/mutation hooks a composition root wires
 * into `Conversations`' props (`onCreateFolder`, `onDeleteFolder`,
 * `onEditFolder`/`onPinFolder`, `sensors`/`handleDragStart`/…,
 * `onMoveToFolderConversation`/…, `onPinConversation`,
 * `isLoadFolders`/…, `onReorderFolders`). The 4 NOT exported:
 * `conversationListState.types.ts` isn't a hook (its 3 types ARE exported,
 * see below); `useDragAndDrop.positioning.ts` is pure arithmetic consumed
 * only by `useDragAndDrop.ts` itself (already re-exports its 2 genuinely
 * reusable position-math functions internally, but those stay off this
 * curated list — the position math is meaningless without the drag
 * context `useDragAndDrop` already provides); `useIsSmallWindow.ts` is a
 * local viewport-breakpoint duplicate `Conversations.tsx` itself calls
 * directly to compute its own collapse behaviour (see that file's own doc
 * comment for why it's duplicated rather than imported) — no future
 * caller needs it independently of `Conversations`, matching this
 * brief's own "purely internal implementation detail" carve-out;
 * `useLatestRef.ts` (added by this same pass, to fix 3 of the 4
 * pre-existing §3.5 `hook-deps` budget breaches this unit's own full-suite
 * verification run surfaced — `useDragAndDrop.ts`'s `handleDragEnd` and
 * `Conversations.renderers.tsx`'s two render-prop factories; the 4th,
 * `ConversationItem.tsx`'s 19-entry `menuItems` dependency array, was
 * fixed by a different technique — splitting into two smaller `useMemo`s,
 * see that file's own doc comment — not by `useLatestRef` — see
 * `useLatestRef.ts`'s own doc comment) is a generic ref-mirroring
 * primitive with no conversation-list-specific meaning at all, not
 * something a composition root would ever call directly.
 * `lib/useHasPermission.ts` (outside `lib/hooks/`, not one of the 13
 * either) is the SAME kind of intra-slice-only primitive
 * `features/pipelines/lib/useHasPermission.ts` already established the
 * precedent for never surfacing through a barrel.
 *
 * **Type-export choices, disclosed.** `ConversationsProps`/
 * `ConversationsFolder` (re-exported by `Conversations.tsx` itself) and
 * `ConversationsDateGroup` (declared alongside them in
 * `Conversations.types.ts` but NOT re-exported by `Conversations.tsx`) are
 * all spent here: a composition root cannot type its own `dateGroups`/
 * `folders`/`setDateGroups`/`setFolders` state against `ConversationsProps`
 * without them. `FolderListItem`/`DateGroupListItem`/`NewFolderDraft`
 * (`./lib/hooks/conversationListState.types.ts`) are ALSO spent — these,
 * not the `Conversations`-local types above, are what every one of the 9
 * hooks' own params/results actually key on (`setFolders: Dispatch<
 * SetStateAction<readonly FolderListItem[]>>`, etc.). Verified assignable
 * both ways: since `ConversationsFolder`/`ConversationsDateGroup` only ADD
 * optional fields over `FolderListItem`/`DateGroupListItem`, a composition
 * root can hold a single `useState<readonly FolderListItem[]>(...)` and
 * pass its setter to BOTH a hook expecting `Dispatch<SetStateAction<
 * readonly FolderListItem[]>>` AND `Conversations`' own `setFolders` prop
 * (which asks for the wider `ConversationsFolder`-typed dispatch) — no
 * second, duplicate state tree needed. Every hook's own `Params`/`Result`
 * interface (`UseCreateFolderParams`, `UseDragAndDropResult`,
 * `MoveConversationResult`, …) is deliberately NOT re-exported here, same
 * call `features/agents/index.ts` (`AgentEditorDeps` et al.) and
 * `features/pipelines/index.ts` (`PipelineEditorDeps` et al.) already made
 * for the identical class of type: a caller passes an object literal /
 * destructures a hook's return value, both checked structurally against
 * the hook's own (unexported) signature with no import required.
 */
export { Conversations } from './ui/conversations/Conversations';
export type { ConversationsDateGroup, ConversationsFolder, ConversationsProps } from './ui/conversations/Conversations.types';

export type { DateGroupListItem, FolderListItem, NewFolderDraft } from './lib/hooks/conversationListState.types';

export { useArchiveConversation } from './lib/hooks/useArchiveConversation.hooks';
export { useConversationActivity } from './lib/hooks/useConversationActivity.hooks';
export { useConversationRail } from './lib/hooks/useConversationRail.hooks';
export { useCreateFolder } from './lib/hooks/useCreateFolder.hooks';
export { useDateGroupExpansion } from './lib/hooks/useDateGroupExpansion.hooks';
export { useDeleteFolder } from './lib/hooks/useDeleteFolder.hooks';
export { useDragAndDrop } from './lib/hooks/useDragAndDrop';
export { useEditFolder } from './lib/hooks/useEditFolder';
export { useForkConversation } from './lib/hooks/useForkConversation.hooks';
export { useMoveToFolderConversation } from './lib/hooks/useMoveToFolderConversation.hooks';
export { usePinConversation } from './lib/hooks/usePinConversation.hooks';
export { useQueryFoldersList } from './lib/hooks/useQueryFoldersList.hooks';
export { useReorderFolders } from './lib/hooks/useReorderFolders';
