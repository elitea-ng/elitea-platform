/**
 * The messages that wait for the running turn (`useSteering`), at the top of the composer (`QueuedMessages`, the
 * same rows as the chat's): Send now stops the turn and sends that message first, Edit takes it off the queue and
 * puts it back into the message box (before what is typed there), Remove takes it off. Once the agent takes a
 * message at its next safe point, it shows in the transcript as the person's message.
 *
 * Below the rows, the last refusal (the agent has the message already, a full queue), or the texts that did not
 * start the next turn.
 */
import type { RefObject } from 'react';

import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { QueuedMessages } from '@/shared/ui/queued-messages';

import type { DraftInput } from './SteeringControls';
import { nextTurnMessage, type Steering } from './useSteering';

/** What the composer's queue needs of `useSteering`. */
export type SteeringQueueBinding = Pick<Steering, 'pending' | 'error' | 'remove' | 'sendNow'>;

export interface SteeringQueueProps {
  queue: SteeringQueueBinding | undefined;
  input: RefObject<DraftInput | null>;
  /** The input has text now (an edited message went back into it). */
  onRestored: () => void;
}

export function SteeringQueue({ queue, input, onRestored }: SteeringQueueProps): React.JSX.Element | null {
  if (queue === undefined) return null;
  const { pending, error } = queue;
  const edit = (steerId: string): void => {
    const text = pending.find((steer) => steer.steerId === steerId)?.text;
    if (text === undefined) return;
    void queue.remove(steerId).then((removed) => {
      const handle = input.current;
      if (!removed || handle === null) return;
      handle.setValue(nextTurnMessage([text, handle.getInputContent()]));
      onRestored();
    });
  };
  return (
    <>
      <QueuedMessages
        testId="steering-queue"
        items={pending.map((steer) => ({ id: steer.steerId, text: steer.text, testId: 'steer-pending' }))}
        onSendNow={(steerId) => void queue.sendNow(steerId)}
        onEdit={edit}
        onRemove={(steerId) => void queue.remove(steerId)}
      />
      {error !== null && (
        <Typography variant="bodySmall" role="alert" data-testid="steer-error" sx={(theme: Theme) => ({ color: theme.vars.palette.error.main })}>
          {error}
        </Typography>
      )}
    </>
  );
}
