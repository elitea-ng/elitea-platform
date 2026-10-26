/**
 * The composer's steering controls (`useSteering`):
 *
 * - "Stop and send", while a turn runs: stop the turn, then send the text as the next turn.
 * - The texts that did not start the next turn (`UnsentSteers`): they go back into the input, so the person can
 *   change them, and "Send again" sends what the input holds.
 */
import { useEffect, useRef, type RefObject } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';

import { t } from '@/shared/i18n';

import { nextTurnMessage, type UnsentSteers } from './useSteering';

/** The part of the input handle these controls use. */
interface DraftInput {
  getInputContent(): string;
  setValue(value: string): void;
}

/**
 * Put the unsent texts into the input once per `id`, before what the person typed since. `onRestored` tells the
 * composer that the input has text now.
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
    const handle = input.current;
    if (id === undefined || text === undefined || handle === null || restored.current === id) return;
    restored.current = id;
    handle.setValue(nextTurnMessage([text, handle.getInputContent()]));
    latestRestored.current();
  }, [input, id, text]);
}

/** "Stop and send": shown while a turn runs, when the page offers it (`shown`). */
export function StopAndSend({ shown, disabled, onClick }: { shown: boolean; disabled: boolean; onClick: () => void }): React.JSX.Element | null {
  if (!shown) return null;
  return (
    <Box sx={{ display: 'flex', justifyContent: 'flex-end' }}>
      <Button size="small" variant="outlined" color="error" data-testid="workspace-stop-and-send" disabled={disabled} onClick={onClick}>
        {t('workspace.steer.stopAndSend', 'Stop and send')}
      </Button>
    </Box>
  );
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
