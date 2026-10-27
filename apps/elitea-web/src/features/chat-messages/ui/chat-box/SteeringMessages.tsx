/**
 * The steering messages of one answer (client contract 1.27), shown in the
 * transcript right under that answer.
 *
 * - `injected`: shown as "Delivered". After a reload the server sends the
 *   same text as an ordinary user message; `visibleSteering` hides the copy
 *   here when its `message_id` is a row of the transcript.
 * - `pending`: someone else's, shown only while the answer runs, with
 *   "Waiting for the next step". The reader's own pending messages are not
 *   here: they are the composer's queue (`useOwnPendingSteering`), with Send
 *   now, Edit and Remove.
 * - `removed` and `turn_ended`: not shown (a `turn_ended` message of this
 *   tab becomes the next message; see `useChatSteering`).
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { Theme } from '@mui/material/styles';

import { STEERING_STATE, visibleSteering, type SteeringItem } from '@elitea/client-core/stream';

import { t } from '@/shared/i18n';

import { useChatSteeringStore } from '../../model/chatSteering.store';

export interface SteeringMessagesProps {
  readonly responseMessageId: string;
  /** The ids of the transcript rows, for the reload dedup. */
  readonly transcriptIds: ReadonlySet<string>;
  /** The answer still runs: pending messages can still be injected. */
  readonly isLive: boolean;
}

const EMPTY: readonly SteeringItem[] = [];

function bubbleSx(theme: Theme) {
  return {
    marginTop: '0.5rem',
    marginLeft: 'auto',
    maxWidth: '85%',
    boxSizing: 'border-box',
    background: theme.vars.palette.background.aiAnswerBkg,
    borderRadius: theme.vars.shape.radiusMd,
    padding: '0.5rem 0.75rem',
  } as const;
}

const textSx = { whiteSpace: 'pre-wrap', overflowWrap: 'break-word', wordBreak: 'break-word' } as const;

function SteeringRow({ item }: { readonly item: SteeringItem }): ReactNode {
  return (
    <Box sx={bubbleSx} data-testid="chat-steering-item" data-state={item.state}>
      <Typography variant="bodyMedium" color="text.secondary" sx={textSx}>{item.content}</Typography>
      <Typography variant="labelSmall" color="text.secondary" component="p" sx={{ marginTop: '0.25rem' }} data-testid="chat-steering-status">
        {item.state === STEERING_STATE.Injected
          ? t('chatMessages.steering.delivered', 'Delivered')
          : t('chatMessages.steering.pending', 'Waiting for the next step')}
      </Typography>
    </Box>
  );
}

export function SteeringMessages({ responseMessageId, transcriptIds, isLive }: SteeringMessagesProps): ReactNode {
  const items = useChatSteeringStore((state) => state.items[responseMessageId] ?? EMPTY);
  const ownIds = useChatSteeringStore((state) => state.own[responseMessageId]?.ids);
  const shown = visibleSteering(items, transcriptIds).filter(
    (item) => item.state === STEERING_STATE.Injected || (isLive && item.state === STEERING_STATE.Pending && ownIds?.includes(item.id) !== true),
  );
  if (shown.length === 0) return null;
  return (
    <Box data-testid="chat-steering-messages">
      {shown.map((item) => (
        <SteeringRow key={item.id} item={item} />
      ))}
    </Box>
  );
}
