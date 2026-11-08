// @ts-nocheck
/**
 * ParticipantItem's two card renderers (normal / attention), extracted to
 * keep `ParticipantItem.tsx` under the 400-line budget (spec §3.5) — same
 * split-to-a-sibling-file precedent this component already established via
 * `ParticipantItem.styles.ts`.
 */
import { Box, Typography } from '@mui/material';

import WarningAmberIcon from '@mui/icons-material/WarningAmber';
import AttachFileIcon from '@mui/icons-material/AttachFile';
import IconButton from '@mui/material/IconButton';

import { t } from '@/shared/i18n';

import ParticipantActions from '../ParticipantActions/ParticipantActions';
import ParticipantWarning from './ParticipantWarning';
import type { participantItemStyles } from './ParticipantItem.styles';

// ---------------------------------------------------------------------------
// Props
// ---------------------------------------------------------------------------

interface CardBaseProps {
  isActive: boolean; collapsed: boolean; displayName: string; iconNode: React.ReactNode;
  isBeingEdited: boolean; s: ReturnType<typeof participantItemStyles>;
  onClickHandler: () => void; onMouseEnter: () => void; onMouseLeave: () => void;
  containerInfoRow: React.ReactNode; participant: Record<string, unknown>;
  onEdit?: (participant: Record<string, unknown>) => void;
  onDelete?: (participant: Record<string, unknown>) => void;
  disabledEdit?: boolean; showEditButton: boolean; hasRemoteMcpLoggedIn?: boolean;
  isHovering: boolean; mcpLoginSlot?: React.ReactNode; mcpLogoutSlot?: React.ReactNode;
}

export interface NormalCardProps extends CardBaseProps {
  entitySettings: Record<string, unknown> | undefined;
  versionName: string;
  isAttachment?: boolean;
  nameRef: React.RefObject<HTMLSpanElement>;
  /** The agent's own context meter (issue 1610); hidden while hovered, where the actions take its place. */
  contextSlot?: React.ReactNode;
  /** The agent's "Lead" badge and lead menu (issue 1612); stays while hovered. */
  leadSlot?: React.ReactNode;
}

export interface AttentionCardProps extends CardBaseProps {
  entityMeta: Record<string, unknown> | undefined;
  shouldDisableThisItem: boolean | undefined; hasMisconfigurationErrors: boolean | undefined;
  mcpIsDisconnected: boolean | undefined; someToolsAreUnavailable: boolean | undefined;
  blockedToolkitNames: string[] | undefined; isVersionUnavailable: boolean | undefined;
  isPublishedAgentGone: boolean | undefined; remoteMcpLoggedOut: boolean | undefined;
  spOAuthLoggedOut: boolean | undefined; spConfig: unknown;
  handleEditClick: (e: React.MouseEvent) => void; isToolkitParticipant: boolean;
  sharepointLoginSlot?: React.ReactNode;
  leadSlot?: React.ReactNode;
}

// ---------------------------------------------------------------------------
// Normal (non-error) card
// ---------------------------------------------------------------------------

export function renderNormalCard({
  isActive, collapsed, displayName, iconNode, entitySettings, versionName, isBeingEdited,
  s, onClickHandler, onMouseEnter, onMouseLeave, containerInfoRow,
  participant, onEdit, onDelete, disabledEdit, showEditButton, hasRemoteMcpLoggedIn,
  isHovering, mcpLoginSlot, mcpLogoutSlot, isAttachment, nameRef, contextSlot, leadSlot,
}: NormalCardProps): React.ReactElement | null {
  return (
    <Box sx={s.normalItemWrapper}>
      <Box onClick={onClickHandler} onMouseEnter={onMouseEnter} onMouseLeave={onMouseLeave} sx={s.contentWrapper(collapsed, isActive)}>
        <Box sx={s.iconBoxNormal(isActive)}>{iconNode}</Box>

        {!collapsed && (
          <Box sx={s.nameWrapper}>
            <Typography variant="bodyMedium" color="text.secondary" ref={nameRef} sx={s.nameContent}>
              {displayName}
              {isAttachment && (
                <IconButton
                  size="small"
                  disabled
                  sx={{ ml: 0.5, width: '1.25rem', height: '1.25rem' }}
                  aria-label={t('chat-participants.participant.attachmentIndicator', 'Attachment manager')}
                >
                  <AttachFileIcon fontSize="small" />
                </IconButton>
              )}
              {entitySettings?.version_id && (
                <Typography
                  variant="bodyMedium"
                  color={isBeingEdited ? 'primary.main' : 'text.primary'}
                  sx={{ flexShrink: 0, maxWidth: '50%', whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }}
                >
                  {isBeingEdited ? t('chat-participants.participant.editing', 'Editing...') : versionName}
                </Typography>
              )}
            </Typography>
          </Box>
        )}

        {!collapsed && leadSlot}
        {!collapsed && !isHovering && contextSlot}

        {!collapsed && !isBeingEdited && (
          <ParticipantActions
            participant={participant}
            onEdit={onEdit}
            onDelete={onDelete}
            disabledEdit={disabledEdit}
            disabledDeleteButton={disabledEdit}
            showButtons={isHovering}
            showEditButton={showEditButton}
            hasRemoteMcpLoggedIn={hasRemoteMcpLoggedIn}
            mcpLoginSlot={mcpLoginSlot}
            mcpLogoutSlot={mcpLogoutSlot}
          />
        )}
      </Box>
      {containerInfoRow}
    </Box>
  );
}

// ---------------------------------------------------------------------------
// Attention (error) card
// ---------------------------------------------------------------------------

export function renderAttentionCard({
  isActive, collapsed, displayName, iconNode, isBeingEdited, entityMeta, s,
  onClickHandler, onMouseEnter, onMouseLeave, containerInfoRow,
  shouldDisableThisItem, hasMisconfigurationErrors, mcpIsDisconnected,
  someToolsAreUnavailable, blockedToolkitNames, isVersionUnavailable,
  isPublishedAgentGone, remoteMcpLoggedOut, spOAuthLoggedOut, spConfig,
  handleEditClick, isToolkitParticipant, participant, onEdit, onDelete,
  disabledEdit, showEditButton, hasRemoteMcpLoggedIn, isHovering,
  mcpLoginSlot, mcpLogoutSlot, sharepointLoginSlot, leadSlot,
}: AttentionCardProps): React.ReactElement {
  return (
    <Box
      onClick={isActive || isVersionUnavailable ? onClickHandler : undefined}
      onMouseEnter={onMouseEnter}
      onMouseLeave={onMouseLeave}
      sx={s.attentionWrapper(isActive)}
    >
      <Box sx={s.attentionHeader}>
        <Box sx={s.iconBoxAttention}>{iconNode}</Box>
        {!collapsed && (
          <Box sx={s.attentionNameBox}>
            <Typography variant="bodyMedium" color="text.secondary" sx={s.attentionDisplayName}>
              {displayName}
            </Typography>
            {isBeingEdited && (
              <Typography variant="bodyMedium" color="primary.main" sx={s.attentionEditingText}>
                {entityMeta?.project_id === 'public' ? t('chat-participants.participant.viewing', 'Viewing...') : t('chat-participants.participant.editing', 'Editing...')}
              </Typography>
            )}
          </Box>
        )}
        {!collapsed && leadSlot}
        {!collapsed && !isBeingEdited && (
          <ParticipantActions
            participant={participant}
            onEdit={onEdit}
            onDelete={onDelete}
            disabledEdit={disabledEdit || isPublishedAgentGone || isVersionUnavailable}
            disabledDeleteButton={disabledEdit}
            showButtons={isHovering}
            showEditButton={showEditButton}
            hasRemoteMcpLoggedIn={hasRemoteMcpLoggedIn}
            mcpLoginSlot={mcpLoginSlot}
            mcpLogoutSlot={mcpLogoutSlot}
          />
        )}
      </Box>
      <Box sx={s.attentionMessageRow}>
        <Box sx={s.attentionIcon}>
          <WarningAmberIcon />
        </Box>
        <Typography variant="bodySmall" color="text.attention" sx={s.attentionMessage}>
          <ParticipantWarning
            isPublishedAgentGone={isPublishedAgentGone}
            isVersionUnavailable={isVersionUnavailable}
            hasMisconfigurationErrors={hasMisconfigurationErrors}
            shouldDisableThisItem={shouldDisableThisItem}
            mcpIsDisconnected={mcpIsDisconnected}
            someToolsAreUnavailable={someToolsAreUnavailable}
            blockedToolkitNames={blockedToolkitNames}
            remoteMcpLoggedOut={remoteMcpLoggedOut}
            spOAuthLoggedOut={spOAuthLoggedOut}
            isSkippedContainer={false}
            handleEditClick={handleEditClick}
            isToolkitParticipant={isToolkitParticipant}
            spConfig={spConfig}
            participant={participant}
            mcpLoginSlot={mcpLoginSlot}
            sharepointLoginSlot={sharepointLoginSlot}
          />
        </Typography>
      </Box>
      {containerInfoRow}
    </Box>
  );
}

export function hasParticipantErrors(props: {
  shouldDisableThisItem?: boolean; hasMisconfigurationErrors?: boolean;
  mcpIsDisconnected?: boolean; remoteMcpLoggedOut?: boolean;
  spOAuthLoggedOut?: boolean; someToolsAreUnavailable?: boolean;
  isVersionUnavailable?: boolean; isPublishedAgentGone?: boolean;
}): boolean {
  return Boolean(
    props.shouldDisableThisItem
    || props.hasMisconfigurationErrors
    || props.mcpIsDisconnected
    || props.remoteMcpLoggedOut
    || props.spOAuthLoggedOut
    || props.someToolsAreUnavailable
    || props.isVersionUnavailable
    || props.isPublishedAgentGone,
  );
}
