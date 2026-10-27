/**
 * The composer's end: the one round accent button (`ComposerSendButton`).
 *
 * - No turn runs: Send.
 * - A turn runs and Send steers it (`steering`): Stop as a plain icon, then Send, which queues the text.
 * - A turn runs otherwise: the accent button is Stop.
 */
import StopRoundedIcon from '@mui/icons-material/StopRounded';
import Box from '@mui/material/Box';

import { t } from '@/shared/i18n';
import { ComposerIconButton, ComposerSendButton } from '@/shared/ui/native-composer';

interface WorkspaceSendControlProps {
  busy: boolean;
  steering: boolean;
  /** The input says Send is off (no session, a cloud thread), or there is nothing to send. */
  disabled: boolean;
  onSend: () => void;
  onStop: () => void;
}

export function WorkspaceSendControl({ busy, steering, disabled, onSend, onStop }: WorkspaceSendControlProps): React.JSX.Element {
  const stopLabel = t('chatInput.userInput.stopGenerating', 'Stop generating');
  const send = <ComposerSendButton testId="workspace-send-button" label={t('workspace.send', 'Send')} disabled={disabled} onClick={onSend} />;
  if (!busy) return send;
  if (!steering) return <ComposerSendButton testId="workspace-stop-button" label={stopLabel} disabled={false} onClick={onStop} icon={<StopRoundedIcon />} />;
  return (
    <Box sx={{ display: 'flex', alignItems: 'center', gap: 0.5 }}>
      <ComposerIconButton label={stopLabel} data-testid="workspace-stop-button" onClick={onStop}>
        <StopRoundedIcon />
      </ComposerIconButton>
      {send}
    </Box>
  );
}
