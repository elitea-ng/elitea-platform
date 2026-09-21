/**
 * The notice a conversation shows when the agent it uses has been withdrawn
 * from the catalogue (#972).
 *
 * Its own file rather than a block inside `ChatBox.tsx`: that component is at
 * its §3.5 file-length and complexity ceilings, and a banner is exactly the
 * kind of leaf this widget already splits out (`ChatBoxDeleteModal`).
 *
 * `role="alert"` because the reader has to be TOLD, not merely shown: the
 * state appears without any action of theirs — someone else withdrew the
 * agent — and a silent caption is what the pre-#972 conversation already
 * effectively was.
 */
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

export interface ChatBoxWithdrawnNoticeProps {
  readonly withdrawn: boolean;
}

export function ChatBoxWithdrawnNotice({ withdrawn }: ChatBoxWithdrawnNoticeProps): React.ReactElement | null {
  if (!withdrawn) return null;
  return (
    <Typography
      role="alert"
      data-testid="chat-participant-withdrawn-notice"
      variant="caption"
      sx={{ px: 2, py: 1 }}
    >
      {t(
        'widgets.chatBox.participantWithdrawn',
        'This agent has been unpublished and is no longer available. You can still read this conversation.',
      )}
    </Typography>
  );
}
