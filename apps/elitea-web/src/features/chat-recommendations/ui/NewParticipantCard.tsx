import { memo, useCallback } from 'react';
import type { ReactNode } from 'react';

import { Box, Tooltip, Typography } from '@mui/material';

import { t } from '@/shared/i18n';

import { EntityIcon } from './EntityIcon';

/**
 * Mirrors `features/chat-participants/model/constants.ts`'s
 * `ChatParticipantType.Applications`/`.Pipelines` values — this feature
 * cannot import that module (`no-sideways-features`), so the wire literals
 * are inlined here instead.
 */
const APPLICATIONS_TYPE = 'applications';
const PIPELINES_TYPE = 'pipelines';

/**
 * Mirrors `features/chat-participants/model/constants.ts`'s env-derived
 * `PUBLIC_PROJECT_ID` (itself mirroring old-app `common/constants.js:14,61`)
 * — same `no-sideways-features` constraint as above.
 */
const VITE_PUBLIC_PROJECT_ID = (import.meta.env.VITE_PUBLIC_PROJECT_ID as string | undefined) ?? '';
const PUBLIC_PROJECT_ID = VITE_PUBLIC_PROJECT_ID || '0';

export type NewParticipantCardProps = {
  participant: {
    id: string;
    name: string;
    description?: string;
    participantType?: string;
    project_id?: string;
    agent_type?: string;
    type?: string;
    icon_meta?: { component?: ReactNode; url?: string };
  };
  onClick: (participant: NewParticipantCardProps['participant']) => void;
  alreadyExists?: boolean;
  isActive?: boolean;
};

/** Ported from `NewParticipantCard.jsx:90-97`. */
function deriveTypeLabel(participant: NewParticipantCardProps['participant']): string {
  if (participant.participantType === APPLICATIONS_TYPE) {
    return participant.agent_type === 'pipeline' ? t('chatRecommendations.card.pipeline', 'pipeline') : t('chatRecommendations.card.agent', 'agent');
  }
  if (participant.type?.toLowerCase().endsWith('mcp')) {
    return t('chatRecommendations.card.mcp', 'MCP');
  }
  return participant.participantType ?? t('chatRecommendations.card.agent', 'agent');
}

/** Ported from `NewParticipantCard.jsx:99-101`. */
function isPublicBadgeVisible(participant: NewParticipantCardProps['participant']): boolean {
  return (
    participant.project_id === PUBLIC_PROJECT_ID &&
    (participant.participantType === APPLICATIONS_TYPE || participant.participantType === PIPELINES_TYPE)
  );
}

/** `entityType` EntityIcon needs — 'user' has no distinct wire `participantType` value from the toolkit/user catalog fallback other than `'user'` itself. */
function deriveEntityIconType(participant: NewParticipantCardProps['participant']): 'agent' | 'pipeline' | 'toolkit' | 'user' {
  if (participant.participantType === 'user') return 'user';
  if (participant.participantType === 'toolkit') return 'toolkit';
  if (participant.agent_type === 'pipeline' || participant.participantType === PIPELINES_TYPE) return 'pipeline';
  return 'agent';
}

const NewParticipantCard = memo(({ participant, onClick, alreadyExists = false, isActive = false }: NewParticipantCardProps) => {
  const onClickHandler = useCallback(
    (e: React.MouseEvent) => {
      e.stopPropagation();
      onClick(participant);
    },
    [onClick, participant],
  );

  return (
    <Tooltip
      enterDelay={1000}
      enterNextDelay={1000}
      placement="top"
      title={
        <>
          <Typography variant="labelSmall" sx={{ fontWeight: 700 }}>{participant.name}</Typography>
          <Typography variant="bodySmall">{participant.description ?? ''}</Typography>
        </>
      }
    >
      <Box
        onClick={onClickHandler}
        sx={(theme) => ({
          display: 'flex',
          alignItems: 'center',
          gap: '0.75rem',
          borderRadius: theme.vars.shape.radiusMd,
          padding: '0.5rem 0.75rem',
          height: '3.5rem',
          cursor: alreadyExists ? 'default' : 'pointer',
          background: isActive ? 'background.userInputBackgroundActive' : 'background.userInputBackground',
          border: alreadyExists ? '1px solid' : 'none',
          borderColor: 'border.userMessageEditor',
          '&:hover': {
            background: 'background.userInputBackgroundActive',
          },
        })}
      >
        <EntityIcon
          icon={participant.icon_meta}
          entityType={deriveEntityIconType(participant)}
        />
        <Box sx={{ flex: 1, minWidth: 0 }}>
          <Typography
            variant="headingSmall"
            color="text.secondary"
            sx={{
              overflow: 'hidden',
              textOverflow: 'ellipsis',
              whiteSpace: 'nowrap',
            }}
          >
            {participant.name}
          </Typography>
          <Typography variant="bodySmall" color="text.default" sx={{ textTransform: 'capitalize' }}>
            {deriveTypeLabel(participant)}
          </Typography>
        </Box>
        {isPublicBadgeVisible(participant) && (
          <Box
            sx={(theme) => ({
              boxSizing: 'border-box',
              display: 'flex',
              flexDirection: 'row',
              alignItems: 'center',
              padding: '0.125rem 0.375rem',
              height: '1.25rem',
              borderRadius: theme.vars.shape.radiusPill,
              border: '1px solid',
              borderColor: 'border.lines',
            })}
          >
            <Typography variant="bodySmall" sx={{ textTransform: 'none', color: 'text.metrics' }}>
              {t('chatRecommendations.card.public', 'Public')}
            </Typography>
          </Box>
        )}
      </Box>
    </Tooltip>
  );
});

NewParticipantCard.displayName = 'NewParticipantCard';

export default NewParticipantCard;
