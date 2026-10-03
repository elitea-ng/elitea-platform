// @ts-nocheck
/**
 * ParticipantSection — collapsible section with title and participant list.
 *
 * Ported from `[fsd]/features/chat/participants/ui/ExpandedParticipants/ParticipantSection.jsx`.
 *
 * Prop shape matches the REAL caller, `ui/ParticipantsLayout.tsx:131-147`
 * (`entityType`/`onSelectParticipant`/`onDeleteParticipant`/`onEditParticipant`/
 * `onUpdateParticipant`/`activeParticipantId`/`disabledAdd`/`resolveToolkitIcon`)
 * — NOT the `title`/`onItemClick`/`onDelete`/`onEdit`/object-shaped
 * `editingToolkit` this file previously declared, which meant every prop the
 * real caller passed landed on a key this component never read, so no
 * section ever rendered a participant (wave-2 C5 adversarial-review finding #1).
 */
import { memo, useCallback, useState } from 'react';

import { Box, Collapse, IconButton, Typography } from '@mui/material';

import ExpandMoreIcon from '@mui/icons-material/ExpandMore';
import ExpandLessIcon from '@mui/icons-material/ExpandLess';

import { t } from '@/shared/i18n';

import ExpandedParticipantsList from './ExpandedParticipantsList';
import type { ExpandedParticipantsListProps } from './ExpandedParticipantsList';

// ---------------------------------------------------------------------------
// Props
// ---------------------------------------------------------------------------

export interface ParticipantSectionProps {
  /** Section grouping label ('Agent' | 'Pipeline' | 'Toolkit' | 'MCP', per `Participants.tsx`'s `deriveEntityTypeName`). */
  entityType: string;
  participants: Record<string, unknown>[];
  collapsed?: boolean;
  disabledEdit?: boolean;
  /** Accepted for parity with the real caller's prop surface; no per-item "add" affordance exists at this level (matches old app's `ParticipantSection.jsx`, which never read it either). */
  disabledAdd?: boolean;
  activeParticipantId?: string;
  onSelectParticipant?: (participant: Record<string, unknown>) => void;
  onDeleteParticipant?: (participant: Record<string, unknown>) => void;
  onEditParticipant?: (participant: Record<string, unknown>) => void;
  /** Accepted for parity with the real caller's prop surface; old app's `ParticipantItem` has no update-in-place affordance. */
  onUpdateParticipant?: (participant: Record<string, unknown>) => void;
  editingToolkit?: Record<string, unknown>;
  resolveToolkitIcon?: ExpandedParticipantsListProps['resolveToolkitIcon'];
  mcpLoginSlot?: React.ReactNode;
  mcpLogoutSlot?: React.ReactNode;
  sharepointLoginSlot?: React.ReactNode;
}

/**
 * `${entityType}s` — matches old app's `ParticipantSection.jsx` title
 * (`${entityType.toLowerCase() !== 'mcp' ? entityType : 'MCP'}s`), e.g.
 * 'Agent' -> 'Agents', 'MCP' -> 'MCPs'.
 */
function sectionTitle(entityType: string): string {
  const suffix = entityType.toLowerCase() === 'mcp' ? 'MCP' : entityType;
  return `${suffix}s`;
}

/**
 * ParticipantSection component — section with collapsible header and participant list.
 */
const ParticipantSection = memo((props: ParticipantSectionProps): React.ReactElement | null => {
  const {
    entityType,
    participants,
    collapsed,
    disabledEdit,
    activeParticipantId,
    onSelectParticipant,
    onDeleteParticipant,
    onEditParticipant,
    editingToolkit,
    resolveToolkitIcon,
    mcpLoginSlot,
    mcpLogoutSlot,
    sharepointLoginSlot,
  } = props;

  const [isExpanded, setIsExpanded] = useState(!collapsed);

  const toggleExpand = useCallback(() => {
    setIsExpanded((prev) => !prev);
  }, []);

  if (!participants?.length) return null;

  return (
    <Box sx={{ mb: 1 }}>
      <Box
        sx={{
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'space-between',
          px: 1,
          py: 0.5,
          cursor: 'pointer',
          userSelect: 'none',
        }}
        onClick={toggleExpand}
      >
        <Typography variant="headingSmall">
          {sectionTitle(entityType)} ({participants.length})
        </Typography>
        <IconButton
          size="small"
          onClick={toggleExpand}
          aria-label={isExpanded ? t('chat-participants.section.collapse', 'Collapse section') : t('chat-participants.section.expand', 'Expand section')}
        >
          {isExpanded ? <ExpandLessIcon fontSize="small" /> : <ExpandMoreIcon fontSize="small" />}
        </IconButton>
      </Box>
      <Collapse in={isExpanded}>
        <Box sx={{ px: 1 }}>
          <ExpandedParticipantsList
            participants={participants}
            collapsed={collapsed}
            disabledEdit={disabledEdit}
            activeParticipantId={activeParticipantId}
            onItemClick={onSelectParticipant}
            onDelete={onDeleteParticipant}
            onEdit={onEditParticipant}
            editingToolkit={editingToolkit}
            resolveToolkitIcon={resolveToolkitIcon}
            mcpLoginSlot={mcpLoginSlot}
            mcpLogoutSlot={mcpLogoutSlot}
            sharepointLoginSlot={sharepointLoginSlot}
          />
        </Box>
      </Collapse>
    </Box>
  );
});

ParticipantSection.displayName = 'ParticipantSection';

export default ParticipantSection;
