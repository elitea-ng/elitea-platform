// @ts-nocheck
import { memo } from 'react';

import { useTheme } from '@mui/material/styles';
import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';

import { useGetCurrentAuthor } from '@/shared/api/generated/social/social';
import type { SocialAuthorProfile } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import type { TransformedParticipant } from '../../model/types';
import { avatarInitialsType, typeScale } from '@/shared/brand/typeScale';

// ---------------------------------------------------------------------------
// ParticipantItemRow — internal helper for the "users" row
// ---------------------------------------------------------------------------

/** Minimal row item for user participants in the expanded header. */
export interface ParticipantItemRowProps {
  readonly participant: TransformedParticipant;
  readonly isActive: boolean;
  readonly onClickItem: (participant: TransformedParticipant) => void;
}

interface RowState {
  readonly name: string;
  readonly isOtherUser: boolean;
  readonly avatarUrl: string | undefined;
  readonly participantId: string;
}

/**
 * Current user's id, resolved via `useGetCurrentAuthor` — same established
 * pattern as `chat-conversation-list/ui/folders/FolderItem.tsx`'s own
 * `useCurrentUserId` (kept local/duplicated here rather than imported: that
 * file lives outside this cluster's edit scope).
 */
function useCurrentUserId(): string | undefined {
  const query = useGetCurrentAuthor();
  return (query.data?.data as SocialAuthorProfile | undefined)?.id;
}

/**
 * Derives this row's display name, self-exclusion, and avatar from the real
 * (snake_case) wire shape.
 *
 * - `name`: `entity_meta.name` first, then `meta.user_name` — matches old
 *   app's `getParticipantName` (`participants.helpers.js:23-44`, Users
 *   branch) and this directory's own `UserParticipantItem.tsx`. Deliberately
 *   NOT delegated to `lib/hooks/useParticipantName` — that hook's
 *   `participantDisplayName` (`entities/participant`) reads CAMELCASE fields
 *   never populated on this feature's snake_case `TransformedParticipant`,
 *   so it always resolves to `''` and then throws on an unimported
 *   `DEFAULT_PARTICIPANT_NAME` (wave-2 C5 adversarial-review finding #2).
 *   Fixing that hook is outside this cluster's file scope
 *   (`lib/hooks/useParticipantName.ts`).
 * - `isOtherUser`: parity with old app's `user?.id != participant_user_id`
 *   (`UserParticipantItem.jsx:20`) — loose-equality semantics preserved: while
 *   the current user id hasn't loaded yet, default to "other user" so the
 *   row stays clickable rather than silently inert.
 * - `avatarUrl`: `meta.user_avatar`, what old app's `UserAvatar
 *   avatar={user_avatar}` actually rendered — NOT `useParticipantEntityIcon`'s
 *   `entity_settings.icon_meta` chain, which is never populated for a
 *   Users-type participant (wave-2 C5 adversarial-review finding #11).
 */
function computeRowState(participant: TransformedParticipant, currentUserId: string | undefined): RowState {
  const entityMeta = participant.entity_meta;
  const meta = participant.meta;

  const name =
    entityMeta?.name ||
    (meta?.user_name as string | undefined) ||
    t('chat-participants.row.defaultUserName', 'User');

  const participantId = entityMeta?.id ?? '';
  const isOtherUser = currentUserId === undefined || String(currentUserId) !== String(participantId);
  const avatarUrl = meta?.user_avatar as string | undefined;

  return { name, isOtherUser, avatarUrl, participantId };
}

/** `default` for your own row (self-mention is a no-op) or while inactive-and-selected; `pointer` otherwise. */
function resolveCursor(isOtherUser: boolean, isActive: boolean): 'default' | 'pointer' {
  if (!isOtherUser) return 'default';
  return isActive ? 'default' : 'pointer';
}

const ParticipantItemRow = memo(
  ({ participant, isActive, onClickItem }: ParticipantItemRowProps) => {
    const theme = useTheme();
    const currentUserId = useCurrentUserId();

    const { name, isOtherUser, avatarUrl, participantId } = computeRowState(participant, currentUserId);
    const cursor = resolveCursor(isOtherUser, isActive);

    // Old app's `UserParticipantItem.jsx:26-28`: clicking your own avatar is a
    // deliberate no-op — you can't @-mention yourself.
    const handleClick = () => {
      if (isOtherUser) onClickItem(participant);
    };

    return (
      <Box
        component="button"
        type="button"
        sx={{
          display: 'flex',
          alignItems: 'center',
          gap: 1,
          background: 'transparent',
          border: 'none',
          cursor,
          padding: '0.25rem 0.5rem',
          borderRadius: 'var(--el-shape-radiusSm, 4px)',
          opacity: isActive ? 1 : 0.85,
          '&:hover': {
            opacity: 1,
            backgroundColor: 'action.hover',
          },
        }}
        onClick={handleClick}
        aria-label={isOtherUser ? t('chat-participants.row.mention', 'Mention {{name}}', { name }) : name}
        data-testid={`participant-item-${participantId}`}
      >
        {avatarUrl ? (
          <Box
            component="img"
            src={avatarUrl}
            alt={name}
            sx={{ width: 20, height: 20, borderRadius: 'var(--el-shape-radiusSm, 4px)' }}
          />
        ) : (
          <Box
            sx={{
              width: 20,
              height: 20,
              borderRadius: 'var(--el-shape-radiusSm, 4px)',
              backgroundColor: 'action.selected',
              display: 'flex',
              alignItems: 'center',
              justifyContent: 'center',
              // A 20px tile: initials snap to the ladder by avatar size.
              ...typeScale(theme.typography[avatarInitialsType(20)]),
              fontWeight: 600,
            }}
          >
            {(name?.[0] ?? '?').toUpperCase()}
          </Box>
        )}
        <Typography
          variant="bodySmall"
          sx={{
            overflow: 'hidden',
            textOverflow: 'ellipsis',
            whiteSpace: 'nowrap',
            maxWidth: '12rem',
          }}
        >
          {name}
        </Typography>
      </Box>
    );
  },
);

ParticipantItemRow.displayName = 'ParticipantItemRow';

export default ParticipantItemRow;
