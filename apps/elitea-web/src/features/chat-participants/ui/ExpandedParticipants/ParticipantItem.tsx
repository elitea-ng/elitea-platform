// @ts-nocheck
/**
 * ParticipantItem — per-participant card (normal or attention/error state).
 * Ported from `[fsd]/features/chat/participants/ui/ExpandedParticipants/ParticipantItem.jsx`.
 *
 * Card rendering lives in `./ParticipantItem.cards.tsx` (split to stay under
 * the 400-line budget, spec §3.5 — same precedent as `ParticipantItem.styles.ts`).
 */
import { memo, useCallback, useEffect, useMemo, useRef, useState } from 'react';

import { Box, Tooltip, Typography } from '@mui/material';
import { useTheme } from '@mui/material/styles';

import InfoIcon from '@mui/icons-material/Info';
import AccountTreeIcon from '@mui/icons-material/AccountTree';

import { t } from '@/shared/i18n';

import { ChatParticipantType } from '../../model/constants';
import { canParticipantBeActiveInChat, isSkippedContainerParticipant } from '../../lib/helpers';
import { useParticipantEntityIcon } from '../../lib/hooks/useParticipantEntityIcon';
import { DEFAULT_PARTICIPANT_NAME } from '@/entities/participant';

import { hasParticipantErrors, renderAttentionCard, renderNormalCard } from './ParticipantItem.cards';
import { participantItemStyles } from './ParticipantItem.styles';

// ---------------------------------------------------------------------------
// Props
// ---------------------------------------------------------------------------

export interface ParticipantItemProps {
  participant: Record<string, unknown>;
  disabledEdit?: boolean;
  collapsed?: boolean;
  isActive?: boolean;
  onClickItem?: (participant: Record<string, unknown>) => void;
  onDelete?: (participant: Record<string, unknown>) => void;
  onEdit?: (participant: Record<string, unknown>) => void;
  editingToolkit?: Record<string, unknown>;
  disableTooltip?: boolean;
  isAttachment?: boolean;
  mcpLoginSlot?: React.ReactNode;
  mcpLogoutSlot?: React.ReactNode;
  sharepointLoginSlot?: React.ReactNode;
  resolveToolkitIcon?: Parameters<typeof useParticipantEntityIcon>[0]['resolveToolkitIcon'];
  // Status flags from ParticipantDetailsContext
  hasMisconfigurationErrors?: boolean;
  someToolsAreUnavailable?: boolean;
  blockedToolkitNames?: string[];
  isPublishedAgentGone?: boolean;
  isVersionUnavailable?: boolean;
  mcpIsDisconnected?: boolean;
  remoteMcpLoggedOut?: boolean;
  hasRemoteMcpLoggedIn?: boolean;
  spOAuthLoggedOut?: boolean;
  spOAuthLoggedIn?: boolean;
  spConfig?: unknown;
  shouldDisableThisItem?: boolean;
}

// ---------------------------------------------------------------------------
// Local, real-wire-value-safe helpers.
//
// This feature's own `ChatParticipantType` constant (`model/constants.ts`)
// previously used plural values ('applications'/'pipelines'/'models'/
// 'users') that did NOT match the real (singular: 'application'/'pipeline'/
// 'llm'/'user') backend wire shape — that has since been fixed at the
// source (wave-2 C5 adversarial-review finding "ENTITY_ORDER wrong casing"/
// the C5-expanded cluster's own follow-up note); code in this file can use
// `ChatParticipantType.*` directly again. `entities/participant`'s
// selectors still read CAMELCASE fields against this feature's snake_case
// data, though — still out of this cluster's file scope to fix
// (`entities/participant/model/selectors.ts`). The helpers below read the
// real wire shape directly so the fixes in this file are actually reachable
// against live data (wave-2 C5 adversarial-review findings #1/#2).
// ---------------------------------------------------------------------------

/**
 * Local port of `getParticipantName` (`participants.helpers.js:23-44`) — NOT
 * `lib/hooks/useParticipantName`, which always resolves `''` against this
 * feature's snake_case data and then throws on an unimported
 * `DEFAULT_PARTICIPANT_NAME` (finding #2). Each old-app per-type branch
 * reads a mutually-exclusive field, so one fallback chain reproduces the
 * switch without a type comparison.
 */
function resolveParticipantDisplayName(participant: Record<string, unknown> | undefined): string {
  if (!participant) return DEFAULT_PARTICIPANT_NAME;
  const entityMeta = participant.entity_meta as Record<string, unknown> | undefined;
  const meta = participant.meta as Record<string, unknown> | undefined;
  return (
    (entityMeta?.name as string | undefined) ||
    (meta?.name as string | undefined) ||
    (entityMeta?.model_name as string | undefined) ||
    (meta?.user_name as string | undefined) ||
    DEFAULT_PARTICIPANT_NAME
  );
}

/**
 * ParticipantItem component — per-participant card (normal or attention state).
 * Memo'd for performance.
 */
const ParticipantItem = memo((props: ParticipantItemProps): React.ReactElement | null => {
  const theme = useTheme();
  const s = participantItemStyles(theme);
  const {
    participant,
    disabledEdit,
    collapsed,
    isActive,
    onClickItem,
    onDelete,
    onEdit,
    editingToolkit,
    disableTooltip,
    isAttachment,
    mcpLoginSlot,
    mcpLogoutSlot,
    sharepointLoginSlot,
    hasMisconfigurationErrors,
    someToolsAreUnavailable,
    blockedToolkitNames,
    isPublishedAgentGone,
    isVersionUnavailable,
    mcpIsDisconnected,
    remoteMcpLoggedOut,
    hasRemoteMcpLoggedIn,
    spOAuthLoggedOut,
    spConfig,
    shouldDisableThisItem,
    resolveToolkitIcon,
  } = props;

  const nameRef = useRef<HTMLSpanElement>(null);
  const [nameIsOverflow, setNameIsOverflow] = useState(false);
  const [isHovering, setIsHovering] = useState(false);

  const entityIcon = useParticipantEntityIcon(participant, { resolveToolkitIcon });
  const displayName = resolveParticipantDisplayName(participant);

  const entityMeta = participant.entity_meta as Record<string, unknown> | undefined;
  const entityName = participant.entity_name as ChatParticipantType | undefined;
  const entitySettings = participant.entity_settings as Record<string, unknown> | undefined;

  // Pipeline detection, now actually applied to icon selection below (was
  // previously computed but unused, reintroducing bug #4993 — finding #9).
  const agentType = entitySettings?.agent_type as string | undefined;
  const isPipelineParticipant = agentType === 'pipeline' || participant.agent_type === 'pipeline';

  // Skipped container agent (issue #5680)
  const isSkippedContainer = useMemo(
    () => !isActive && isSkippedContainerParticipant(participant),
    [isActive, participant],
  );

  // Being-edited state
  const isBeingEdited = useMemo(() => {
    const isActiveToolKit = entityName === ChatParticipantType.Toolkits;
    return isActiveToolKit && editingToolkit?.entity_meta?.id === entityMeta?.id;
  }, [entityName, editingToolkit, entityMeta]);

  const canBeActive = useMemo(() => canParticipantBeActiveInChat(participant), [participant]);
  const isToolkitParticipant = entityName === ChatParticipantType.Toolkits;
  const showEditButton = entityName === ChatParticipantType.Toolkits || entityName === ChatParticipantType.Applications;

  // Custom icon_meta image when present, else a distinct icon for pipeline
  // participants (fixes bug #4993), else the initials fallback.
  const iconNode = useMemo(() => {
    if (entityIcon.url) {
      return <img src={entityIcon.url} alt={displayName} style={{ width: '100%', height: '100%', objectFit: 'cover' }} />;
    }
    if (isPipelineParticipant) {
      return <AccountTreeIcon fontSize="small" />;
    }
    return (displayName[0] ?? '?').toUpperCase();
  }, [entityIcon.url, displayName, isPipelineParticipant]);

  // Version name
  const [versionName, setVersionName] = useState('');
  useEffect(() => {
    const versions = entitySettings?.versions as Array<{ id: string; name: string }> | undefined;
    const versionId = entitySettings?.version_id as string | undefined;
    if (versions && versionId) {
      setVersionName(versions.find((v) => v.id === versionId)?.name || '');
    } else {
      setVersionName('');
    }
  }, [entitySettings]);

  const onClickHandler = useCallback(() => {
    if (!disabledEdit && (isActive || canBeActive)) {
      onClickItem?.(isActive ? undefined : participant);
    }
  }, [disabledEdit, isActive, onClickItem, participant, canBeActive]);

  const handleEditClick = useCallback(
    (e: React.MouseEvent) => {
      e.preventDefault();
      e.stopPropagation();
      onEdit?.(participant);
    },
    [onEdit, participant],
  );

  // Overflow detection
  useEffect(() => {
    if (!isHovering && nameRef.current) {
      setNameIsOverflow(nameRef.current.scrollWidth > nameRef.current.clientWidth);
    }
  }, [isHovering]);

  // Auto-deselect when this active participant becomes misconfigured or its
  // published agent disappears (previously dropped entirely — finding #12).
  useEffect(() => {
    if ((hasMisconfigurationErrors || isPublishedAgentGone) && isActive) {
      onClickItem?.(undefined);
    }
  }, [isActive, onClickItem, hasMisconfigurationErrors, isPublishedAgentGone]);

  const containerInfoRow = !collapsed && isSkippedContainer ? (
    <Box sx={s.infoMessageRow}>
      <Box sx={s.infoIcon}>
        <InfoIcon />
      </Box>
      <Typography variant="bodySmall" color="text.secondary" sx={s.attentionMessage}>
        {t(
          'chat-participants.warning.skippedContainer',
          'Its sub-agent chain is at the nesting limit (3 tiers), so it runs only as the active agent here — select it to run.',
        )}
      </Typography>
    </Box>
  ) : null;

  const hasErrors = hasParticipantErrors({
    shouldDisableThisItem,
    hasMisconfigurationErrors,
    mcpIsDisconnected,
    remoteMcpLoggedOut,
    spOAuthLoggedOut,
    someToolsAreUnavailable,
    isVersionUnavailable,
    isPublishedAgentGone,
  });

  const onMouseEnter = () => setIsHovering(true);
  const onMouseLeave = () => setIsHovering(false);

  const content = hasErrors
    ? renderAttentionCard({
      isActive, collapsed, displayName, iconNode, isBeingEdited, entityMeta,
      s, onClickHandler, onMouseEnter, onMouseLeave, containerInfoRow,
      shouldDisableThisItem, hasMisconfigurationErrors, mcpIsDisconnected,
      someToolsAreUnavailable, blockedToolkitNames, isVersionUnavailable,
      isPublishedAgentGone, remoteMcpLoggedOut, spOAuthLoggedOut, spConfig,
      handleEditClick, isToolkitParticipant, participant, onEdit, onDelete,
      disabledEdit, showEditButton, hasRemoteMcpLoggedIn, isHovering,
      mcpLoginSlot, mcpLogoutSlot, sharepointLoginSlot,
    })
    : renderNormalCard({
      isActive, collapsed, displayName, iconNode, entitySettings, versionName, isBeingEdited,
      s, onClickHandler, onMouseEnter, onMouseLeave, containerInfoRow,
      participant, onEdit, onDelete, disabledEdit, showEditButton, hasRemoteMcpLoggedIn,
      isHovering, mcpLoginSlot, mcpLogoutSlot, isAttachment, nameRef,
    });

  if (disableTooltip) return content;

  // Hover/collapsed-mode tooltip with the full name + version — previously
  // dropped entirely (finding #13).
  return (
    <Tooltip
      title={collapsed || nameIsOverflow ? `${displayName} - ${versionName}` : ''}
      placement="left"
      enterDelay={1000}
    >
      {content}
    </Tooltip>
  );
});

ParticipantItem.displayName = 'ParticipantItem';

export default ParticipantItem;
