/**
 * The composer's end: the one round accent button (`ComposerSendButton`), as the chat's.
 *
 * - No turn runs: Send.
 * - A turn runs: Stop (a square; Esc Esc does the same, `useDoubleEscape`). When Send steers the turn (`steering`)
 *   and there is text, it is Send, which queues the text.
 */
import StopRoundedIcon from '@mui/icons-material/StopRounded';

import { t } from '@/shared/i18n';
import { ComposerSendButton } from '@/shared/ui/native-composer';

interface WorkspaceSendControlProps {
  busy: boolean;
  steering: boolean;
  /** The input holds no text. */
  empty: boolean;
  /** The input says Send is off (no session, a cloud thread). */
  disabled: boolean;
  onSend: () => void;
  onStop: () => void;
}

export function WorkspaceSendControl({ busy, steering, empty, disabled, onSend, onStop }: WorkspaceSendControlProps): React.JSX.Element {
  if (busy && (!steering || empty)) {
    return (
      <ComposerSendButton
        testId="workspace-stop-button"
        label={t('chatInput.userInput.stopGenerating', 'Stop generating')}
        tooltip={t('chatInput.userInput.stopShortcut', 'Stop · Esc Esc')}
        disabled={false}
        onClick={onStop}
        icon={<StopRoundedIcon />}
      />
    );
  }
  return <ComposerSendButton testId="workspace-send-button" label={t('workspace.send', 'Send')} disabled={disabled || empty} onClick={onSend} />;
}
