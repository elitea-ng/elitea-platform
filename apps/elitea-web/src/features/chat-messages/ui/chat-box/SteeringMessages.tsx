/**
 * The steering messages of one answer (client contract 1.26), shown in the
 * transcript right under that answer.
 *
 * - `pending`: shown only while the answer runs, with "Waiting for the next
 *   step". The author can edit it, remove it, or stop the turn and send it
 *   ("Stop and send": the end-of-turn rule then sends it as the next
 *   message).
 * - `injected`: shown as "Delivered". After a reload the server sends the
 *   same text as an ordinary user message; `visibleSteering` hides the copy
 *   here when its `message_id` is a row of the transcript.
 * - `removed` and `turn_ended`: not shown (a `turn_ended` message of this
 *   tab becomes the next message; see `useChatSteering`).
 */
import type { ReactNode } from 'react';
import { useState } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import IconButton from '@mui/material/IconButton';
import TextField from '@mui/material/TextField';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';
import type { Theme } from '@mui/material/styles';
import CloseIcon from '@mui/icons-material/Close';
import EditOutlinedIcon from '@mui/icons-material/EditOutlined';

import { STEERING_STATE, isSteeringTextValid, visibleSteering, type SteeringItem } from '@elitea/client-core/stream';

import { t } from '@/shared/i18n';

import { useChatSteeringStore } from '../../model/chatSteering.store';
import type { ChatSteeringModel } from '../../model/useChatSteering';

export interface SteeringMessagesProps {
  readonly responseMessageId: string;
  /** The ids of the transcript rows, for the reload dedup. */
  readonly transcriptIds: ReadonlySet<string>;
  /** The answer still runs: pending messages can still be injected. */
  readonly isLive: boolean;
  readonly model: ChatSteeringModel;
  /** Stops the running turn; the pending messages then go as the next message. */
  readonly onStopAndSend?: (() => void) | undefined;
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
const footerSx = { display: 'flex', alignItems: 'center', gap: '0.25rem', marginTop: '0.25rem' } as const;

interface RowProps {
  readonly item: SteeringItem;
  readonly own: boolean;
  readonly model: ChatSteeringModel;
  readonly onStopAndSend?: (() => void) | undefined;
}

function SteeringRow({ item, own, model, onStopAndSend }: RowProps): ReactNode {
  const [draft, setDraft] = useState<string | null>(null);
  const pending = item.state !== STEERING_STATE.Injected;
  const save = () => {
    if (draft === null || !isSteeringTextValid(draft)) return;
    void model.edit(item, draft).finally(() => { setDraft(null); });
  };
  if (draft !== null) {
    return (
      <Box sx={bubbleSx} data-testid="chat-steering-edit">
        <TextField
          multiline
          fullWidth
          size="small"
          value={draft}
          onChange={(event) => { setDraft(event.target.value); }}
          slotProps={{ htmlInput: { 'aria-label': t('chatMessages.steering.editLabel', 'Edit the waiting message') } }}
        />
        <Box sx={footerSx}>
          <Button size="small" variant="contained" disabled={!isSteeringTextValid(draft)} onClick={save}>
            {t('chatMessages.steering.save', 'Save')}
          </Button>
          <Button size="small" variant="outlined" onClick={() => { setDraft(null); }}>
            {t('chatMessages.steering.cancel', 'Cancel')}
          </Button>
        </Box>
      </Box>
    );
  }
  const editLabel = t('chatMessages.steering.edit', 'Edit');
  const removeLabel = t('chatMessages.steering.remove', 'Remove');
  return (
    <Box sx={bubbleSx} data-testid="chat-steering-item" data-state={item.state}>
      <Typography variant="bodyMedium" color="text.secondary" sx={textSx}>{item.content}</Typography>
      <Box sx={footerSx}>
        <Typography variant="labelSmall" color="text.secondary" sx={{ flex: 1 }} data-testid="chat-steering-status">
          {pending
            ? t('chatMessages.steering.pending', 'Waiting for the next step')
            : t('chatMessages.steering.delivered', 'Delivered')}
        </Typography>
        {pending && own && (
          <>
            <Tooltip title={editLabel} placement="top">
              <IconButton size="small" aria-label={editLabel} data-testid="chat-steering-edit-button" onClick={() => { setDraft(item.content); }}>
                <EditOutlinedIcon fontSize="small" />
              </IconButton>
            </Tooltip>
            <Tooltip title={removeLabel} placement="top">
              <IconButton size="small" aria-label={removeLabel} data-testid="chat-steering-remove" onClick={() => { void model.remove(item); }}>
                <CloseIcon fontSize="small" />
              </IconButton>
            </Tooltip>
            {onStopAndSend && (
              <Button size="small" data-testid="chat-steering-stop-and-send" onClick={onStopAndSend}>
                {t('chatMessages.steering.stopAndSend', 'Stop and send')}
              </Button>
            )}
          </>
        )}
      </Box>
    </Box>
  );
}

export function SteeringMessages({ responseMessageId, transcriptIds, isLive, model, onStopAndSend }: SteeringMessagesProps): ReactNode {
  const items = useChatSteeringStore((state) => state.items[responseMessageId] ?? EMPTY);
  const ownIds = useChatSteeringStore((state) => state.own[responseMessageId]?.ids);
  const shown = visibleSteering(items, transcriptIds).filter(
    (item) => item.state === STEERING_STATE.Injected || (isLive && item.state === STEERING_STATE.Pending),
  );
  if (shown.length === 0) return null;
  return (
    <Box data-testid="chat-steering-messages">
      {shown.map((item) => (
        <SteeringRow
          key={item.id}
          item={item}
          own={ownIds?.includes(item.id) === true}
          model={model}
          onStopAndSend={onStopAndSend}
        />
      ))}
    </Box>
  );
}
