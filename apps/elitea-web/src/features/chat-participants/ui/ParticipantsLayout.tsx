// @ts-nocheck
/**
 * ParticipantsLayout — renders the expanded participants container, sections,
 * and context-budget slot.
 *
 * Extracted from `Participants.tsx` to keep that file under 400 lines.
 * Prop budget (≤ 12 §3.5) is maintained by grouping related props into objects.
 *
 * Composition-root fixes (adversarial review C5-wrapper #3, #4, #9):
 *  - Type-sections render the real `ParticipantItem` directly, not the
 *    disconnected `ExpandedParticipants/ParticipantSection` stub (underscore-
 *    prefixed destructuring, never rendered any rows). Status flags come
 *    from `useParticipantDetailsContext()`, mounted by `ParticipantsWrapper`.
 *  - When collapsed on a large window, renders a collapsed icon-strip
 *    (`CollapsedParticipantsDropdown` per type) instead of the sections list —
 *    mirroring old-app `Participants.jsx`'s `showCollapsedParticipants`.
 *  - The users row caps to 3 avatars when the panel's rendered width is
 *    narrow (<= 200px), matching old-app `componentWidth <= 200 ?
 *    users.slice(0, 3) : users.slice(0, 5)`, via `ResizeObserver`.
 *  - Uses `../lib/helpers`'s `getChatParticipantUniqueId`, not
 *    `@/entities/participant`'s camelCase-keyed one — the latter always
 *    misses on this feature's snake_case shape and returns the same id for
 *    every participant, colliding as duplicate React `key`s. See
 *    `lib/helpers.ts`'s doc comment.
 */
import { memo, useCallback, useEffect, useRef, useState } from 'react';

import Box from '@mui/material/Box';
import Collapse from '@mui/material/Collapse';
import IconButton from '@mui/material/IconButton';
import Typography from '@mui/material/Typography';

import ExpandLessIcon from '@mui/icons-material/ExpandLess';
import ExpandMoreIcon from '@mui/icons-material/ExpandMore';
import PersonAddAltOutlinedIcon from '@mui/icons-material/PersonAddAltOutlined';

import { t } from '@/shared/i18n';

import type { TransformedParticipant } from '../model/types';
import { getChatParticipantUniqueId } from '../lib/helpers';
import { useParticipantDetailsContext } from '../lib/context/ParticipantDetailsContext';
import { styles } from './ExpandedParticipants/participants.styles';
import ParticipantItem from './ExpandedParticipants/ParticipantItem';
import { UsersRow } from './ExpandedParticipants/UsersRow';
import { CollapsedParticipantsStrip } from './ParticipantsLayout.CollapsedStrip';
import type { ParticipantsProps } from './Participants.types';

// Grouped prop interfaces (§3.5 component-props budget)

interface HeaderState {
  showTitle: boolean;
  collapseIcon: React.ReactNode;
  collapsed: boolean;
  onCollapsed?: () => void;
  /** True when the collapsed icon-strip replaces the sections list (`collapsed && !isSmallWindow`). */
  showCollapsedParticipants: boolean;
}

interface UserDisplay {
  usersToDisplay: TransformedParticipant[];
  hasOverflow: boolean;
  visibleCount: number;
  maxVisibleUsers: number;
}

interface SectionConfig {
  sections: Array<{ key: string; type: string; participants: TransformedParticipant[]; entityType: string }>;
  activeParticipantId?: string;
  disabledEdit?: boolean;
  disabledAdd?: boolean;
  /** Id of the toolkit acting as the active conversation's attachment manager. */
  selectedManager?: string;
  /** Same as `selectedManager`, for a conversation still being composed. */
  newConversationSelectedManager?: string;
}

interface ParticipantActions {
  onSelectParticipant?: (p: TransformedParticipant) => void;
  onDeleteParticipant?: (p: TransformedParticipant) => void;
  onEditParticipant?: (p: TransformedParticipant) => void;
  onUpdateParticipant?: (p: TransformedParticipant) => void;
  editingToolkit?: string;
  resolveToolkitIcon?: ParticipantsProps['resolveToolkitIcon'];
  /** Opens the consumer's add-participant picker — see `ParticipantsProps`. */
  onAddParticipants?: () => void;
}

export interface ParticipantsLayoutProps {
  header: HeaderState;
  users: UserDisplay;
  sections: SectionConfig;
  actions: ParticipantActions;
  renderContextBudget?: ParticipantsProps['renderContextBudget'];
  /**
   * The conversation the budget slot is for.
   *
   * It used to be absent, and the slot was invoked as
   * `renderContextBudget({ conversationId: undefined })` — a literal, so the
   * budget could never fetch for ANY conversation no matter what the caller
   * passed. Nothing supplied `renderContextBudget` at the time, so the defect
   * was invisible: the slot was dead, and the value it fed was wrong.
   */
  conversationId?: string | number | undefined;
}

/**
 * Maps a section's grouping key (a real `ChatParticipantType` value, e.g.
 * `'application'`/`'pipeline'`/`'toolkit'`/`'mcp'` — singular, since the
 * `model/constants.ts` plural/singular mismatch was fixed) to the existing
 * `chat-participants.collapsed.*` i18n keys — those labels ("Agents",
 * "Pipelines", "Toolkits", "MCPs") already exist in `en.json` for the
 * (previously unwired) collapsed icon-strip, and are the same labels
 * old-app pluralizes onto its expanded section headers.
 */
const SECTION_LABEL_KEYS: Record<string, string> = {
  application: 'chat-participants.collapsed.agents',
  pipeline: 'chat-participants.collapsed.pipelines',
  toolkit: 'chat-participants.collapsed.toolkits',
  mcp: 'chat-participants.collapsed.mcps',
};

/**
 * ParticipantTypeSection — collapsible per-type section rendering real
 * `ParticipantItem` rows (replaces the dead `ExpandedParticipants/ParticipantSection`).
 */
interface ParticipantTypeSectionProps {
  title: string;
  participants: TransformedParticipant[];
  collapsed: boolean;
  disabledEdit?: boolean;
  activeParticipantId?: string;
  selectedManager?: string;
  newConversationSelectedManager?: string;
  onSelectParticipant?: (p: TransformedParticipant) => void;
  onDeleteParticipant?: (p: TransformedParticipant) => void;
  onEditParticipant?: (p: TransformedParticipant) => void;
  editingToolkit?: string;
  resolveToolkitIcon?: ParticipantsProps['resolveToolkitIcon'];
}

const ParticipantTypeSection = memo((props: ParticipantTypeSectionProps) => {
  const {
    title, participants, collapsed, disabledEdit, activeParticipantId,
    selectedManager, newConversationSelectedManager,
    onSelectParticipant, onDeleteParticipant, onEditParticipant,
    editingToolkit, resolveToolkitIcon,
  } = props;

  const [isExpanded, setIsExpanded] = useState(true);
  const { getParticipantStatus } = useParticipantDetailsContext();
  const toggleExpand = useCallback(() => setIsExpanded((prev) => !prev), []);

  if (!participants.length) return null;

  return (
    <Box sx={{ width: '100%' }} data-testid={`participants-section-${title}`}>
      <Box
        sx={{
          display: 'flex', alignItems: 'center', justifyContent: 'space-between',
          width: '100%', cursor: 'pointer', userSelect: 'none', padding: '.25rem 0',
        }}
        onClick={toggleExpand}
      >
        {/* component="p", not the variant's default <h6>: this is a section
            label inside the rail, and an h6 under the page's heading tree
            skips levels — axe heading-order, which fired the moment a
            journey first attached a participant. */}
        <Typography variant="subtitle2" component="p" color="text.secondary">
          {title} ({participants.length})
        </Typography>
        <IconButton
          size="small"
          onClick={(event) => { event.stopPropagation(); toggleExpand(); }}
          aria-label={
            isExpanded
              ? t('chat-participants.section.collapse', 'Collapse section')
              : t('chat-participants.section.expand', 'Expand section')
          }
        >
          {isExpanded ? <ExpandLessIcon fontSize="small" /> : <ExpandMoreIcon fontSize="small" />}
        </IconButton>
      </Box>
      <Collapse in={isExpanded}>
        <Box sx={{ display: 'flex', flexDirection: 'column', gap: 0.5, width: '100%' }}>
          {participants.map((participant) => {
            const uniqueId = getChatParticipantUniqueId(participant);
            const status = getParticipantStatus(
              participant.entity_name,
              participant.entity_meta?.id ?? '',
              participant.entity_meta?.project_id ?? '',
            );
            return (
              <ParticipantItem
                key={uniqueId}
                participant={participant}
                disabledEdit={disabledEdit}
                collapsed={collapsed}
                isActive={activeParticipantId === uniqueId}
                isAttachment={(newConversationSelectedManager || selectedManager) === participant.entity_meta?.id}
                onClickItem={onSelectParticipant}
                onDelete={onDeleteParticipant}
                onEdit={onEditParticipant}
                editingToolkit={editingToolkit}
                resolveToolkitIcon={resolveToolkitIcon}
                {...status}
              />
            );
          })}
        </Box>
      </Collapse>
    </Box>
  );
});

ParticipantTypeSection.displayName = 'ParticipantTypeSection';

/**
 * Width-responsive users cap (adversarial review C5-wrapper #9): mirrors
 * old-app `useGetComponentWidth()` via `ResizeObserver`. Extracted to its own
 * hook to keep `ParticipantsLayout`'s own complexity within the §3.5 budget.
 */
function useContainerWidth(containerRef: React.RefObject<HTMLDivElement | null>): number {
  const [width, setWidth] = useState(0);

  useEffect(() => {
    const node = containerRef.current;
    if (!node || typeof ResizeObserver === 'undefined') return undefined;
    const observer = new ResizeObserver((entries) => {
      const entry = entries[0];
      if (entry) setWidth(entry.contentRect.width);
    });
    observer.observe(node);
    return () => observer.disconnect();
  }, [containerRef]);

  return width;
}

/** old-app: `componentWidth <= 200 ? users.slice(0, 3) : users.slice(0, 5)`. */
function deriveEffectiveMaxVisibleUsers(componentWidth: number, maxVisibleUsers: number): number {
  if (componentWidth > 0 && componentWidth <= 200) return Math.min(3, maxVisibleUsers);
  return maxVisibleUsers;
}

/** ExpandedParticipantsContent — users row + type sections + empty state (same complexity-budget reason as the strip above). */
interface ExpandedParticipantsContentProps {
  users: UserDisplay;
  sections: SectionConfig;
  actions: ParticipantActions;
  usersToRender: TransformedParticipant[];
  usersOverflowCount: number;
}

function ExpandedParticipantsContent({
  users, sections, actions, usersToRender, usersOverflowCount,
}: ExpandedParticipantsContentProps) {
  return (
    <>
      {/* Users row (always at top when visible) */}
      {users.visibleCount > 0 && (
        <UsersRow
          usersToRender={usersToRender}
          overflowCount={usersOverflowCount}
          activeParticipantId={sections.activeParticipantId}
          onSelectParticipant={actions.onSelectParticipant!}
          onDeleteParticipant={actions.onDeleteParticipant}
          disabledEdit={sections.disabledEdit}
        />
      )}

      {/* Type sections — real ParticipantItem rows. `collapsed` is hard-false
          here (matching old-app's hard-coded `collapsed={false}` on
          `ParticipantSection`): this only renders when NOT showing the
          collapsed strip, so items always render expanded. */}
      {sections.sections.map(({ key, participants: group, entityType }) => (
        <ParticipantTypeSection
          key={key}
          title={t(SECTION_LABEL_KEYS[key] ?? `chat-participants.collapsed.${key}`, `${entityType}s`)}
          participants={group}
          collapsed={false}
          disabledEdit={sections.disabledEdit}
          activeParticipantId={sections.activeParticipantId}
          selectedManager={sections.selectedManager}
          newConversationSelectedManager={sections.newConversationSelectedManager}
          onSelectParticipant={actions.onSelectParticipant}
          onDeleteParticipant={actions.onDeleteParticipant}
          onEditParticipant={actions.onEditParticipant}
          editingToolkit={actions.editingToolkit}
          resolveToolkitIcon={actions.resolveToolkitIcon}
        />
      ))}

      {/* Empty state when no sections and not collapsed */}
      {sections.sections.length === 0 && users.visibleCount === 0 && (
        <Typography variant="bodySmall" color="text.secondary" sx={styles.emptyState}>
          {t('chat-participants.expanded.noParticipants', 'No participants')}
        </Typography>
      )}
    </>
  );
}

// Component (≤ 12 props via grouping)

export function ParticipantsLayout({
  header, users, sections, actions, renderContextBudget, conversationId,
}: ParticipantsLayoutProps) {
  const containerRef = useRef<HTMLDivElement | null>(null);
  const componentWidth = useContainerWidth(containerRef);
  const effectiveMaxVisibleUsers = deriveEffectiveMaxVisibleUsers(componentWidth, users.maxVisibleUsers);
  const usersToRender = users.usersToDisplay.slice(0, effectiveMaxVisibleUsers);
  const usersOverflowCount = users.visibleCount - effectiveMaxVisibleUsers;

  return (
    <Box ref={containerRef} sx={styles.mainContainer(header.collapsed)} data-testid="participants-container">
      {/* Content area */}
      <Box sx={styles.contentContainer(header.collapsed)}>
        {/* Header */}
        <Box sx={styles.headerContainer(header.collapsed)}>
          {header.showTitle && (
            <Typography variant="subtitle" sx={styles.titleText}>
              {t('chat-participants.participants.title', 'Participants')}
            </Typography>
          )}
          {/*
            * The add-participant control (gap G2).
            *
            * `disabledAdd` — the playback flag AND the caller's
            * `configuration.users.users.view` grant — has been computed and
            * threaded through this rail since the port landed, and there was
            * nothing for it to disable: the picker it gates
            * (`ui/chat-modal/AddNewUserModal.tsx`) had no call site anywhere
            * in the app, so a conversation could gain an AGENT from the
            * composer and could never gain a person at all. Rendered only
            * when the consumer supplies the callback, so the rail's other
            * mount points do not grow a control that opens nothing.
            */}
          {!header.collapsed && actions.onAddParticipants && (
            <IconButton
              sx={styles.collapseButton}
              size="small"
              disabled={sections.disabledAdd === true}
              onClick={actions.onAddParticipants}
              data-testid="participants-add-button"
              aria-label={t('chat-participants.participants.addParticipants', 'Add participants')}
            >
              <PersonAddAltOutlinedIcon fontSize="small" />
            </IconButton>
          )}
          {header.onCollapsed && (
            <IconButton
              sx={styles.collapseButton}
              size="small"
              onClick={header.onCollapsed}
              aria-label={header.collapsed ? t('chat-participants.expand', 'Expand participants') : t('chat-participants.collapse', 'Collapse participants')}
            >
              {header.collapseIcon}
            </IconButton>
          )}
        </Box>

        {/* Participants sections */}
        <Box sx={styles.participantsContainer(header.collapsed)}>
          {header.showCollapsedParticipants ? (
            <CollapsedParticipantsStrip users={users} sections={sections} actions={actions} />
          ) : (
            <ExpandedParticipantsContent
              users={users}
              sections={sections}
              actions={actions}
              usersToRender={usersToRender}
              usersOverflowCount={usersOverflowCount}
            />
          )}
        </Box>
      </Box>

      {/* Context budget slot */}
      {renderContextBudget && (
        <Box sx={styles.contextBudgetWrapper}>
          {renderContextBudget({ conversationId, collapsed: header.showCollapsedParticipants })}
        </Box>
      )}
    </Box>
  );
}

ParticipantsLayout.displayName = 'ParticipantsLayout';
