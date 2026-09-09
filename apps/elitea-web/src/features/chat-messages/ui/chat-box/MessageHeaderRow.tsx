/**
 * The caption line every transcript row carries above its bubble:
 * `<avatar> <author> to <recipient>` on the left, the relative timestamp
 * hard right.
 *
 * Ported from the `verticalMode` header both baseline rows render — user
 * side `UserMessage.jsx:128-158` (`styles.headerBox` / `styles.avatarContainer`
 * / `styles.userName` / `styles.sentToName`), assistant side
 * `ApplicationAnswer.jsx:604-648` (`styles.headerRow` / `styles.headerLeft` /
 * `styles.participantName` / `styles.replyToText` / `styles.timeWrapper`).
 * The two are the same row with different avatars and a different recipient
 * label, so they share one component here rather than two near-identical
 * copies.
 *
 * `verticalMode` itself is not a prop: the chat page only ever renders the
 * vertical layout (the horizontal one is the agent-configuration modal's,
 * which has its own component), so the row is unconditional.
 */
import type { ReactNode } from 'react';

import PsychologyOutlinedIcon from '@mui/icons-material/PsychologyOutlined';
import Box from '@mui/material/Box';
import Chip from '@mui/material/Chip';
import Tooltip from '@mui/material/Tooltip';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import { CreatedTimeInfo } from '../CreatedTimeInfo';

/** @public Props for `MessageHeaderRow`. */
export interface MessageHeaderRowProps {
  /** The 24px avatar node rendered at the far left (a user avatar, or the brand mark for an assistant). */
  readonly avatar: ReactNode;
  /** The author's display name. */
  readonly name: string;
  /** Who the message was sent to — rendered after a literal "to". Omitted, the whole clause is dropped (baseline gates it on `sentTo?.entity_name`). */
  readonly sentToName?: string | undefined;
  /** Underlines the recipient and gives it a pointer, matching the baseline's clickable `replyToText`/`sentToName`. */
  readonly sentToInteractive?: boolean;
  /** Click handler for the recipient — only wired when `sentToInteractive`. */
  readonly onSentToClick?: (() => void) | undefined;
  /** The message's ISO creation time; renders the right-aligned relative label. */
  readonly createdAt?: string | undefined;
  /**
   * How many of the caller's persistent memories (#870) this turn's recall
   * used. Renders a small "Using N memories" chip left of the timestamp;
   * absent (not `0`) whenever the turn used none, matching
   * `AssistantMessage.memoriesUsed`'s own "absent means none" contract.
   */
  readonly memoriesUsed?: number | undefined;
}

const headerRowSx = {
  display: 'flex',
  flexDirection: 'row',
  justifyContent: 'space-between',
  alignItems: 'center',
  width: '100%',
  maxWidth: '100%',
  // baseline: `padding: '0 0.25rem 0 0.25rem'`
  padding: '0 0.25rem',
  flexWrap: 'nowrap',
  overflow: 'hidden',
  gap: '0.5rem',
} as const;

const headerLeftSx = {
  display: 'flex',
  flexDirection: 'row',
  alignItems: 'center',
  // baseline `headerLeft` uses 0.625rem; the user side's `avatarContainer`
  // uses 0.5rem. 0.625rem is the one the production screenshots measure at.
  gap: '0.625rem',
  flex: 1,
  minWidth: 0,
  overflow: 'hidden',
} as const;

const nameSx = {
  whiteSpace: 'nowrap',
  overflow: 'hidden',
  textOverflow: 'ellipsis',
  flexShrink: 1,
  minWidth: 0,
  maxWidth: '60%',
} as const;

function recipientSx(interactive: boolean) {
  return {
    textDecoration: interactive ? 'underline' : 'none',
    cursor: interactive ? 'pointer' : 'default',
    whiteSpace: 'nowrap',
    flexShrink: 0,
  } as const;
}

export function MessageHeaderRow({
  avatar,
  name,
  sentToName,
  sentToInteractive = false,
  onSentToClick,
  createdAt,
  memoriesUsed,
}: MessageHeaderRowProps): ReactNode {
  return (
    <Box sx={headerRowSx} data-testid="chat-message-header">
      <Box sx={headerLeftSx}>
        {avatar}
        <Typography
          variant="bodySmall"
          sx={(theme: Theme) => ({ ...nameSx, color: theme.vars.palette.text.secondary })}
        >
          {name}
        </Typography>
        {sentToName !== undefined && sentToName !== '' && (
          <>
            <Typography variant="bodySmall">{t('features.chatMessages.sentTo', 'to')}</Typography>
            <Typography
              variant="bodySmall"
              sx={recipientSx(sentToInteractive)}
              onClick={sentToInteractive ? onSentToClick : undefined}
              data-testid="chat-message-recipient"
            >
              {sentToName}
            </Typography>
          </>
        )}
      </Box>
      <Box sx={{ flexShrink: 0, display: 'flex', alignItems: 'center', gap: '0.375rem' }}>
        {memoriesUsed !== undefined && memoriesUsed > 0 && (
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
        {createdAt !== undefined && createdAt !== '' && <CreatedTimeInfo createdAt={createdAt} />}
      </Box>
    </Box>
  );
}
