import type { Conversation } from '@/entities/conversation';
import type { ControlsDropdownLeafItem } from '@/shared/ui/ControlsDropdown';

/**
 * Shared leaf type for the `ConversationItem.*` file cluster
 * (`ConversationItem.tsx`/`.menu.tsx`/`.row.tsx`) — split out to its own
 * file so `.menu.tsx`/`.row.tsx` can depend on it without a circular import
 * back to `ConversationItem.tsx` (which itself imports FROM `.menu.tsx`/
 * `.row.tsx`).
 *
 * `entities/conversation`'s `Conversation` now models `authorId`. The row
 * menu's author-only guard needs it, and the normaliser used to drop it. It
 * still does not model `users_count`/`isNew` — a disclosed gap already flagged by this same
 * feature's own `lib/hooks/useQueryFoldersList.hooks.ts` module doc
 * (`toConversation`'s doc comment, citing `ConversationItem.jsx:57-66`:
 * "destructures `name`/`is_private`/`users_count`/`author_id`/… directly
 * off exactly this list-row shape... a future phase should either widen
 * `FolderConversationRef` to the real wire shape or give this list-row
 * concept its own narrower type"). This IS that future phase. Widened here
 * via intersection rather than editing `entities/conversation/model/
 * types.ts` (out of this unit's scope, and one leaf feature widening a
 * shared entity type on its own say-so would just relocate the same
 * disclosed-gap problem) — baseline: `ConversationItem.jsx:56-66`.
 * `isNew` is `DraftConversation`'s own field (a narrower, differently-shaped
 * sibling of `Conversation`); folded in here as optional so one prop type
 * covers both the ordinary and not-yet-persisted-draft rows this component
 * actually receives (baseline reads `isNew` off whichever it's given,
 * `ConversationItem.jsx:63,78,323-351,469-503`).
 */
export interface ConversationWithOwnerMeta extends Conversation {
  readonly usersCount?: number | undefined;
  readonly isNew?: boolean | undefined;
}

/**
 * The two formats the export route serves (issue 851). Moved here (from
 * `ConversationItem.menu.tsx`, its original home) alongside `ConversationItemProps`
 * below for the identical §3.5 400-line-budget reason — both files (`.tsx`
 * and `.menu.tsx`) need this type, and putting it on either one directly
 * would recreate the exact `.tsx`<->`.menu.tsx` cycle this file's own module
 * doc already exists to avoid.
 */
export type ConversationExportFormat = 'md' | 'json';

/**
 * `ConversationItem.tsx`'s full prop list (issue 940/A6: moved here, out of
 * that file, purely to keep it under the §3.5 400-line budget once the
 * `onDuplicate` prop pushed it over — no behavioural reason, same class of
 * split this whole file already exists for).
 *
 * Every optional field below is `?: T | undefined`, not just `?: T` — this
 * codebase's `tsconfig.json` sets `exactOptionalPropertyTypes: true`, under
 * which `field?: T` means the KEY may be absent but, if present, may not be
 * `undefined`. `Conversations.tsx`'s `renderConversationItem` passes several
 * of these straight through from its OWN optional props via plain JSX
 * attributes (`projectId={projectId}`, etc.) — an always-present attribute
 * whose value happens to be `undefined` — so the explicit `| undefined` is
 * required, not decorative. Same convention already established throughout
 * `shared/ui` (e.g. `StateVariableIconButtonProps`, `FolderAccordionProps`).
 */
export interface ConversationItemProps {
  readonly conversation: ConversationWithOwnerMeta;
  readonly onSelectConversation: (conversation: ConversationWithOwnerMeta) => void;
  readonly isActive?: boolean | undefined;
  readonly onDelete: (conversation: ConversationWithOwnerMeta) => void;
  /**
   * Downloads this conversation's transcript in the named format (issue 851).
   * Still optional, and still what decides whether the Export row is live:
   * the row is disabled when no exporter is supplied, so a surface that does
   * not offer export shows a disabled entry rather than live options wired to
   * nothing.
   */
  readonly onExport?: ((format: ConversationExportFormat) => void) | undefined;
  readonly onEdit: (conversation: ConversationWithOwnerMeta) => void;
  readonly onPlayback: (conversation: ConversationWithOwnerMeta) => void;
  readonly onPin: (conversation: ConversationWithOwnerMeta, shouldPin: boolean) => void;
  /** Issue 940/A6 — clones this conversation into a new, independent one and selects it. */
  readonly onDuplicate: (conversation: ConversationWithOwnerMeta) => void;
  readonly onCreateConversation: (conversation: ConversationWithOwnerMeta) => Promise<unknown>;
  readonly onCancelCreate: () => void;
  readonly onChangeActiveConversationName: (name: string) => void;
  readonly moveToFoldersMenuItems?: readonly ControlsDropdownLeafItem[] | undefined;
  /**
   * Plain boolean PROP (baseline: `ConversationItem.jsx:50` already has it
   * as a prop too, not a `useSelector` read) — the caller (eventually a
   * page/composition-root unit) supplies it from `shared/lib/editorState.ts`'s
   * `useEditorStateStore`; this leaf component never reads that store itself.
   */
  readonly isEditingCanvas?: boolean | undefined;
  readonly enableDragAndDrop?: boolean | undefined;
  readonly isDragDisabled?: boolean | undefined;
  readonly isNextItemHovered?: boolean | undefined;
  readonly onItemHover?: ((itemId: string, isHovered: boolean) => void) | undefined;
  /**
   * N4 signature deviation (explicit param instead of an internal
   * `useSelectedProjectId()`/Redux read) — also doubles as the param
   * `useHasPermission` needs for the "Move to" item's permission check and
   * the param `handleShareConversation` needs to build the share link.
   */
  readonly projectId?: string | undefined;
  /**
   * Baseline: `const { id: userId, personal_project_id } = useSelector(state
   * => state.user);` (`ConversationItem.jsx:74`) — turned into three
   * explicit props (this one, `personalProjectId`, `publicProjectId` below),
   * same substitution instruction as `isEditingCanvas` above.
   * `currentUserId` is ALSO what `Conversations.jsx:82`'s own `state.user`
   * read resolves to (`userId`), so `Conversations.tsx` threads the exact
   * same prop value into both its own `getMoveConversationToFoldersMenuItems`
   * ownership check and this component.
   */
  readonly currentUserId?: string | undefined;
  readonly personalProjectId?: string | number | undefined;
  /**
   * Baseline: the module-level `PUBLIC_PROJECT_ID` constant
   * (`common/constants.js:14,61`, `+VITE_PUBLIC_PROJECT_ID`) — per
   * `entities/project/model/selectors.ts`'s own `isPublicProject` doc
   * comment, this is a per-deployment runtime-config value, not an
   * invented in-package constant, so it is a required parameter here too
   * (reused via `isPublicProject`, not re-derived).
   */
  readonly publicProjectId?: string | number | undefined;
  /**
   * DEPENDENCY-INJECTION DEVIATION (deliberate, documented) — same class
   * `shared/ui/CopyToClipboardButton.tsx`'s own doc comment already
   * establishes: "no shared toast infrastructure yet" (grepped, confirmed
   * again for this unit). The baseline calls `useToast().toastInfo(...)`
   * directly after copying the share link; this takes an `onShareLinkCopied`
   * callback instead so the caller decides how to surface it.
   */
  readonly onShareLinkCopied?: (() => void) | undefined;
  /**
   * Baseline: `getBasename()` from `@/routes` (`ConversationItem.jsx:34,155`).
   * `app/providers/basename.ts`'s `getAppBasename()` is the new equivalent,
   * but `features/` may not import from `app/` (R-L1, strict downward
   * layering) — an explicit prop instead, matching this file's own N4
   * convention for `projectId`. Defaults to `''` (root-relative), the same
   * fallback `getAppBasename()` itself resolves to outside a real router.
   */
  readonly basename?: string | undefined;
}
