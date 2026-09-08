// @ts-nocheck
/**
 * ui/Participants.tsx — Groups participants by type into sections.
 *
 * Ported from `apps/elitea-ui/src/[fsd]/features/chat/participants/ui/Participants.jsx`
 * (old-app) and `ExpandedParticipantsList.jsx` / `ParticipantSection.jsx` (FSD
 * sub-components), adapted to the new-app component model:
 *  - `participants` comes from the consumer (the chat page component), not
 *    fetched internally.
 *  - MCP visibility is gate-checked via an optional `isMcpVisible` prop so
 *    this unit does not import `features/mcp` or `shared/lib/hooks` directly.
 *  - Context-budget / instructions rendering is injected via the
 *    `renderContextBudget` slot (same pattern as `features/pipelines/ui/
 *    ChatPanel.tsx`'s `renderContextBudget`).
 *  - `ParticipantDetailsProvider` is NOT rendered here — it must be provided
 *    by the wrapper so consumers can control which participant set feeds the
 *    detail cache.
 *
 * Cross-cutting gaps:
 *  - `renderContextBudget` — slot for the context-budget widget
 *    (`@/[fsd]/widgets/context-budget`). See `features/pipelines/ui/
 *    ChatPanel.tsx` for the exact shape and usage pattern.
 *  - `resolveToolkitIcon` — optional slot to resolve toolkit/MCP icons.
 *    Falls back to a generic icon when not provided.
 */

import { memo, useCallback, useMemo } from 'react';

import KeyboardDoubleArrowLeftIcon from '@mui/icons-material/KeyboardDoubleArrowLeft';
import KeyboardDoubleArrowRightIcon from '@mui/icons-material/KeyboardDoubleArrowRight';

import { ChatParticipantType } from '../model/constants';
import type { TransformedParticipant } from '../model/types';
import { ParticipantsLayout } from './ParticipantsLayout';
import type { ParticipantsProps } from './Participants.types';
import { ENTITY_ORDER } from './Participants.types';

/**
 * `mcp.helpers.js:7-14`'s `isMcpToolkitType`, duplicated here for the same
 * reason `useSlashMention.ts`'s copy is — no shared home exists in this
 * worktree. `type === 'mcp'` or a `mcp_*` pre-built type.
 */
function isMcpToolkitType(type: string): boolean {
  return type === 'mcp' || type.startsWith('mcp_');
}

/** Order in which participant types appear in the expanded list. */
const entityOrder: typeof ENTITY_ORDER = ENTITY_ORDER;

// ---------------------------------------------------------------------------
// Helper: derive display type name (complexity ≤ 5)
// ---------------------------------------------------------------------------

/** Maps a ChatParticipantType value to its display section name. */
function deriveEntityTypeName(type: string): string {
  if (type === 'mcp') return 'MCP';
  if (type === ChatParticipantType.Applications) return 'Agent';
  if (type === ChatParticipantType.Pipelines) return 'Pipeline';
  if (type === ChatParticipantType.Toolkits) return 'Toolkit';
  if (type === ChatParticipantType.Users) return 'Users';
  return String(type);
}

// ---------------------------------------------------------------------------
// Participants
// ---------------------------------------------------------------------------

/**
 * Main participants list component. Groups participants by type into
 * `ParticipantSection` rows and renders a header with title and collapse
 * controls.
 *
 * The component is fully memoized via `React.memo` — re-renders only when
 * the reference to `props.participants` or any handler callback changes.
 *
 * @see ParticipantsWrapper for the provider wrapper
 * @see ParticipantSection for section-level rendering
 */
export const Participants = memo(
  ({
    participants,
    collapsed = false,
    onCollapsed,
    disabledEdit,
    disabledAdd,
    onAddParticipants,
    activeParticipantId,
    onSelectParticipant,
    onDeleteParticipant,
    onEditParticipant,
    onUpdateParticipant,
    editingToolkit,
    resolveToolkitIcon,
    isMcpVisible = false,
    renderContextBudget,
    conversationId,
    maxVisibleUsers = 5,
    isSmallWindow = false,
    selectedManager,
    newConversationSelectedManager,
  }: ParticipantsProps) => {
    // Mirrors old-app `Participants.jsx`'s `showTitle`/`showCollapsedParticipants`
    // (adversarial review C5-wrapper #4): on a small window the full section
    // list always shows (with title), regardless of the `collapsed` toggle —
    // the collapsed icon-strip is a large-screen-only affordance.
    const showTitle = !collapsed || isSmallWindow;
    const showCollapsedParticipants = collapsed && !isSmallWindow;
    // An ELEMENT, not a component reference. `ParticipantsLayout` types this
    // slot as `React.ReactNode` and renders `{header.collapseIcon}` directly,
    // so handing it the component threw "Objects are not valid as a React
    // child (found: object with keys {$$typeof, type, compare})" — the memo
    // wrapper MUI puts around its icons — and took the whole rail down. It
    // never surfaced because nothing mounted this subtree; the crash arrived
    // with the first call site, not with the code.
    const collapseIcon = collapsed
      ? <KeyboardDoubleArrowLeftIcon fontSize="small" />
      : <KeyboardDoubleArrowRightIcon fontSize="small" />;

    // -----------------------------------------------------------------------
    // Group participants by type
    // -----------------------------------------------------------------------

    const groupedByType = useMemo(() => {
      const groups: Record<string, TransformedParticipant[]> = {};

      for (const p of participants) {
        const entityName = p.entity_name;
        const entitySettings = p.entity_settings ?? {};
        const meta = p.meta ?? {};

        let key: string = entityName;

        // Pipelines are a subtype of applications (agent_type = "pipeline")
        if (
          entityName === ChatParticipantType.Applications &&
          entitySettings.agent_type === ChatParticipantType.Pipelines
        ) {
          key = ChatParticipantType.Pipelines;
        }

        // MCP toolkits are a subtype of toolkits (toolkit_type matches "mcp")
        if (
          entityName === ChatParticipantType.Toolkits &&
          (isMcpToolkitType(entitySettings.toolkit_type) ||
            meta.mcp === true)
        ) {
          key = 'mcp';
        }

        // Filter out MCP toolkits when MCP visibility is off
        if (key === 'mcp' && !isMcpVisible) continue;

        if (!groups[key]) groups[key] = [];
        groups[key].push(p);
      }

      return groups;
    }, [participants, isMcpVisible]);

    // -----------------------------------------------------------------------
    // Users section — extract and limit
    // -----------------------------------------------------------------------

    /*
     * Baseline `ExpandedParticipantsList.jsx:50-56`: the users row is the
     * `entity_name === Users` group, sorted, and nothing else. Membership of
     * `groupedByType[Users]` IS that filter.
     *
     * This used to `.filter(isParticipantStillActive)` first, and the row
     * could therefore never render at all. Two independent reasons, either
     * one fatal:
     *  1. `entities/participant`'s selector keys off the camelCase DOMAIN
     *     shape (`entityName`), and every row here is the raw snake_case
     *     wire row (`entity_name`) the conversation payload carries — the
     *     same mismatch `../lib/helpers.ts`'s CORRECTION note already
     *     records for this cluster's other two selector imports. It matched
     *     nothing and answered falsy for every row.
     *  2. Even on the right shape it answers `false` for `user` BY DESIGN —
     *     the baseline's only caller is the last-message Regenerate gate
     *     (`ChatMessageWrapper.jsx:148`), where "a user is not something a
     *     turn can be re-addressed to" is the correct answer. It is a
     *     re-addressability predicate, not a visibility one.
     * Measured on the E2E stack before the fix: a conversation with one user
     * participant rendered `participants-container` and its agent section and
     * no `users-section` at all.
     *
     * Copied before sorting — `groupedByType`'s arrays are memoised and
     * `Array.prototype.sort` mutates in place.
     */
    const userParticipants = useMemo(
      () =>
        [...(groupedByType[ChatParticipantType.Users] ?? [])].sort((a, b) => {
          const aId = Number(a.entity_meta?.id ?? 0);
          const bId = Number(b.entity_meta?.id ?? 0);
          return aId - bId;
        }),
      [groupedByType],
    );

    const usersToDisplay = useMemo(
      () => userParticipants.slice(0, maxVisibleUsers),
      [userParticipants, maxVisibleUsers],
    );

    const visibleCount = userParticipants.length;
    const hasOverflow = visibleCount > maxVisibleUsers;

    // -----------------------------------------------------------------------
    // Build ordered sections for rendering
    // -----------------------------------------------------------------------

    const sections = useMemo(() => {
      const result: Array<{ key: string; type: string; participants: TransformedParticipant[]; entityType: string }> = [];

      for (const type of entityOrder) {
        const key = typeof type === 'string' ? type : String(type);
        const group = groupedByType[key];
        if (!group || group.length === 0) continue;

        const entityType = deriveEntityTypeName(type);

        result.push({ key, type, participants: group, entityType });
      }

      return result;
    }, [groupedByType]);

    // -----------------------------------------------------------------------
    // Handlers — prevent re-creation
    // -----------------------------------------------------------------------

    const handleSelectParticipant = useCallback(
      (p: TransformedParticipant) => {
        onSelectParticipant?.(p);
      },
      [onSelectParticipant],
    );

    // -----------------------------------------------------------------------
    // Render
    // -----------------------------------------------------------------------

    return (
      <ParticipantsLayout
        header={{ showTitle, collapseIcon, collapsed, onCollapsed, showCollapsedParticipants }}
        users={{ usersToDisplay, hasOverflow, visibleCount, maxVisibleUsers }}
        sections={{
          sections,
          activeParticipantId,
          disabledEdit,
          disabledAdd,
          selectedManager,
          newConversationSelectedManager,
        }}
        actions={{
          onSelectParticipant: handleSelectParticipant,
          onDeleteParticipant,
          onEditParticipant,
          onUpdateParticipant,
          editingToolkit,
          resolveToolkitIcon,
          onAddParticipants,
        }}
        renderContextBudget={renderContextBudget}
        conversationId={conversationId}
      />
    );
  },
);

Participants.displayName = 'Participants';
