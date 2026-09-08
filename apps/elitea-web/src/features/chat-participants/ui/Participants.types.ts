// @ts-nocheck
/**
 * ui/Participants.types.ts — shared type definitions for the chat-participants
 * participants feature.
 *
 * Extracted from `Participants.tsx` to break the circular import between
 * `Participants.tsx` and `ParticipantsLayout.tsx`.
 */
import type { TransformedParticipant } from '../model/types';
import { ChatParticipantType } from '../model/constants';

/**
 * Order in which non-user participant types appear as sections below the
 * users row. Fixed from capitalized display labels ('Users', 'Applications',
 * 'Pipelines', 'Toolkits') to the real lowercase `ChatParticipantType` wire
 * values (adversarial review C5-wrapper #2) — `groupedByType` in
 * `Participants.tsx` keys its groups by `entity_name`/derived type, which are
 * always lowercase, so a capitalized order list never matched any group.
 *
 * `Users` is intentionally NOT included here: users are rendered via their
 * own dedicated row (`userParticipants`/`usersToDisplay` in `Participants.tsx`),
 * matching old-app `ExpandedParticipantsList.jsx`'s `ENTITY_SECTIONS`, which
 * also excludes Users for the same reason. Including it here would double-
 * render the users group (once as the row, once as a redundant "Users"
 * section).
 */
export const ENTITY_ORDER: string[] = [
  ChatParticipantType.Applications,
  ChatParticipantType.Pipelines,
  ChatParticipantType.Toolkits,
  ChatParticipantType.MCP,
];

export interface ParticipantsProps {
  /** The participants array, owned by the consumer. */
  readonly participants: TransformedParticipant[];
  /** When true, show the collapsed (icon-only) row instead of sections. */
  readonly collapsed?: boolean;
  /** Callback to toggle collapsed state. */
  readonly onCollapsed?: () => void;
  /** When truthy, all editing operations are disabled. */
  readonly disabledEdit?: boolean;
  /** When truthy, the "add participant" affordance is disabled. */
  readonly disabledAdd?: boolean;
  /**
   * Opens the consumer's add-participant picker.
   *
   * `disabledAdd` has always been here and has always been threaded down;
   * the control it gates never existed, so the flag decided the state of
   * nothing. The panel renders the affordance only when this callback is
   * supplied — the rail is also mounted by surfaces (the pipeline editor's
   * own panel) that have no user picker to open.
   */
  readonly onAddParticipants?: (() => void) | undefined;
  /** Currently active participant id (used for highlighting the LLM). */
  readonly activeParticipantId?: string;
  /** Called when a participant is selected as the active LLM participant. */
  readonly onSelectParticipant?: (participant: TransformedParticipant) => void;
  /** Called to remove a participant from the chat. */
  readonly onDeleteParticipant?: (participant: TransformedParticipant) => void;
  /** Called to edit participant settings. */
  readonly onEditParticipant?: (participant: TransformedParticipant) => void;
  /** Called when a participant's settings are updated. */
  readonly onUpdateParticipant?: (participant: TransformedParticipant) => void;
  /** The toolkit currently being edited (controls which edit button is highlighted). */
  readonly editingToolkit?: string;
  /**
   * Optional slot to resolve toolkit/MCP icons. Falls back to a generic icon.
   * @see useParticipantEntityIcon
   */
  readonly resolveToolkitIcon?: Parameters<typeof useParticipantEntityIcon>[0]['resolveToolkitIcon'];
  /**
   * When true, this unit assumes MCP toolkits are visible and should not
   * filter them out. Set to `false` by default so MCP toolkits are grouped
   * separately; the consumer may override this via a context or prop.
   */
  readonly isMcpVisible?: boolean;
  /**
   * Slot for rendering the context-budget widget beneath the participants.
   * Receives `{ conversationId, contextStrategy, setActiveConversation,
   * conversationInstructions, persona }` — matching `features/pipelines/ui/
   * ChatPanel.tsx`'s `renderContextBudget` contract.
   *
   * This slot is the mechanism for rendering `@/[fsd]/widgets/context-budget`
   * without importing the `widgets/` layer (no-upward-from-features).
   */
  /**
   * The conversation the `renderContextBudget` slot is for. `ParticipantsLayout`
   * used to hand that slot a literal `undefined`, so the budget could never
   * fetch — see that file's own note.
   */
  readonly conversationId?: string | number | undefined;
  readonly renderContextBudget?: (props: {
    conversationId: string | number | undefined;
    /**
     * True while the rail is showing its 3.25rem collapsed strip. The widget
     * must render its MINIMAL form there — baseline
     * `Participants.jsx:120` passes exactly this
     * (`collapsed && !isSmallWindow`) to `ContextBudgetInfo`, which switches
     * to `ContextBudgetCollapsed`. Without it the full card was rendered into
     * a 44px-wide rail, where every line wrapped and the panel overflowed
     * past the right edge of the viewport.
     */
    collapsed?: boolean;
    contextStrategy?: Record<string, unknown>;
    setActiveConversation?: (update: unknown) => void;
    conversationInstructions?: string;
    persona?: unknown;
  }) => ReactNode;
  /**
   * Maximum number of user participants to show in the collapsed-row header
   * before adding a count indicator. Defaults to `5`.
   */
  readonly maxVisibleUsers?: number;
  /**
   * True when the viewport itself is narrow (mirrors old-app `useIsSmallWindow`).
   * Overrides `collapsed` for both the header title and the collapsed
   * icon-strip switch — on a small window the full section list always
   * shows (with title), even if `collapsed` is true. Defaults to `false`.
   */
  readonly isSmallWindow?: boolean;
  /**
   * Id of the toolkit participant currently acting as the active
   * conversation's attachment manager. Compared against each participant's
   * `entity_meta.id` to flag the "this toolkit is the attachment manager"
   * indicator (`ParticipantItem`'s `isAttachment`).
   */
  readonly selectedManager?: string;
  /** Same as `selectedManager`, but for a conversation still being composed (not yet persisted). */
  readonly newConversationSelectedManager?: string;
}
