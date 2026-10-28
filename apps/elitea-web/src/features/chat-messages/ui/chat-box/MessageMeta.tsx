/**
 * The muted tail of a message's action row (`ReplyActionsRow`): the "Using N
 * memories" chip a reply carries, then the relative timestamp. Both used to
 * sit hard right on a caption line above the bubble
 * (`<avatar> <author> to <recipient>`); the chat now reads as a
 * conversation — the reader's bubble on the right, replies as plain text on
 * the left — so they ride with the row and show when it shows.
 */
import type { ReactNode } from 'react';

import PsychologyOutlinedIcon from '@mui/icons-material/PsychologyOutlined';
import Box from '@mui/material/Box';
import Chip from '@mui/material/Chip';
import type { Theme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';

import { t } from '@/shared/i18n';

import { CreatedTimeInfo } from '../CreatedTimeInfo';

/** @public Props for `MessageMeta`. */
export interface MessageMetaProps {
  /** The message's ISO time; renders the relative label. */
  readonly createdAt?: string | undefined;
  /**
   * How many of the caller's persistent memories (#870) this turn's recall
   * used. Absent (not `0`) whenever the turn used none, matching
   * `AssistantMessage.memoriesUsed`'s own "absent means none" contract.
   */
  readonly memoriesUsed?: number | undefined;
}

export function MessageMeta({ createdAt, memoriesUsed }: MessageMetaProps): ReactNode {
  const hasMemories = memoriesUsed !== undefined && memoriesUsed > 0;
  const hasTime = createdAt !== undefined && createdAt !== '';
  if (!hasMemories && !hasTime) return null;
  return (
    <Box
      sx={(theme: Theme) => ({
        display: 'flex',
        alignItems: 'center',
        gap: 0.75,
        marginLeft: 0.5,
        color: theme.vars.palette.text.metrics,
      })}
    >
      {hasMemories && (
        <Tooltip
          title={t('features.chatMessages.memoriesUsedTooltip', 'Elitea used {{count}} of your saved memories for this answer', { count: memoriesUsed })}
        >
          <Chip
            icon={<PsychologyOutlinedIcon fontSize="small" />}
            label={t('features.chatMessages.memoriesUsed', 'Using {{count}} memories', { count: memoriesUsed })}
            size="small"
            variant="outlined"
            data-testid="chat-message-memories-used"
          />
        </Tooltip>
      )}
      {hasTime && <CreatedTimeInfo createdAt={createdAt} />}
    </Box>
  );
}
