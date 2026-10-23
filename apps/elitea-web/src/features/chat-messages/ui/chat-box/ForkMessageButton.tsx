/**
 * "Fork from here" (client contract 1.11, `forkConversation`): one icon
 * button in a message's action row, user and assistant messages alike. It
 * opens a NEW conversation holding the history up to and including this
 * message. The list renders it only for a settled message (see
 * `ChatMessageList`), so it is never offered on a reply still streaming or
 * waiting for an approval.
 */
import type { ReactNode } from 'react';

import CallSplitOutlinedIcon from '@mui/icons-material/CallSplitOutlined';
import IconButton from '@mui/material/IconButton';
import Tooltip from '@mui/material/Tooltip';

import { t } from '@/shared/i18n';

export function ForkMessageButton({ onFork, disabled = false }: { readonly onFork: () => void; readonly disabled?: boolean }): ReactNode {
  const label = t('features.chatMessages.forkFromHere', 'Fork from here');
  return (
    <Tooltip title={label} placement="top">
      <span>
        <IconButton size="small" color="tertiary" disabled={disabled} onClick={onFork} aria-label={label} data-testid="message-fork">
          <CallSplitOutlinedIcon fontSize="small" />
        </IconButton>
      </span>
    </Tooltip>
  );
}
