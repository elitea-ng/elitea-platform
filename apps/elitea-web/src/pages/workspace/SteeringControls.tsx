/**
 * The texts that did not start the next turn (`UnsentSteers`, `useSteering`): they go back into the input, so the
 * person can change them, and "Send again" sends what the input holds.
 */
import { useEffect, useRef, type RefObject } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';

import { t } from '@/shared/i18n';

import { nextTurnMessage, type UnsentSteers } from './useSteering';

/** The part of the input handle these controls (and `SteeringQueue`'s Edit) use. */
export interface DraftInput {
  getInputContent(): string;
  setValue(value: string): void;
}

/**
 * Put the unsent texts into the input once per `id`, before what the person typed since. `onRestored` tells the
 * composer that the input has text now. A tick later than the effect: the input restores the thread's saved draft
 * (`draftKey`) in its own effect, and this puts the texts before that draft instead of over it.
 */
export function useRestoredDraft(input: RefObject<DraftInput | null>, unsent: UnsentSteers | undefined, onRestored: () => void): void {
  const restored = useRef<number | null>(null);
  const latestRestored = useRef(onRestored);
  useEffect(() => {
    latestRestored.current = onRestored;
  });
  const id = unsent?.id;
  const text = unsent?.text;
  useEffect(() => {
    if (id === undefined || text === undefined || restored.current === id) return undefined;
    const timer = window.setTimeout(() => {
      const handle = input.current;
      if (handle === null) return;
      restored.current = id;
      handle.setValue(nextTurnMessage([text, handle.getInputContent()]));
      latestRestored.current();
    }, 0);
    return () => window.clearTimeout(timer);
  }, [input, id, text]);
}

/** "Send again": shown when texts did not start the next turn and no turn runs. It sends what the input holds. */
export function RetryUnsent(props: { unsent: UnsentSteers | undefined; busy: boolean; hasDraft: boolean; canSend: boolean; onClick: () => void }): React.JSX.Element | null {
  const { unsent, busy, hasDraft, canSend, onClick } = props;
  if (unsent === undefined || busy) return null;
  return (
    <Box sx={{ display: 'flex', justifyContent: 'flex-end' }}>
      <Button size="small" variant="outlined" data-testid="workspace-steer-retry" disabled={!hasDraft || !canSend} onClick={onClick}>
        {t('workspace.steer.retry', 'Send again')}
      </Button>
    </Box>
  );
}
