/**
 * The action row under a question (`UserMessage`), drawn as the reply's row
 * (`ApplicationAnswerActions`): one compact, left-aligned row OUTSIDE the
 * bubble, quiet 16px outlined icons (`ReplyActionButton`), Copy · Edit ·
 * Fork · Delete. Each renders only when its handler is supplied.
 *
 * It used to sit inside the card, right-aligned, at MUI's default
 * icon-button weight. Now it fades in when the question is hovered
 * (`replyActionsRevealSx` on the row's message) or focus enters it, and is
 * always shown on a device with no hover — faded by opacity, so Tab still
 * reaches every control.
 *
 * Not exported from this feature's `index.ts` barrel: `UserMessage` is its
 * only consumer.
 */
import type { ReactNode } from 'react';

import CallSplitOutlinedIcon from '@mui/icons-material/CallSplitOutlined';
import ContentCopyIcon from '@mui/icons-material/ContentCopy';
import DeleteOutlinedIcon from '@mui/icons-material/DeleteOutlined';
import EditOutlinedIcon from '@mui/icons-material/EditOutlined';

import { t } from '@/shared/i18n';

import { ReplyActionButton, ReplyActionsRow } from './ReplyActionButton';

export function UserMessageActions({
  onCopy,
  onEdit,
  onDelete,
  onFork,
}: {
  readonly onCopy?: (() => void) | undefined;
  readonly onEdit?: (() => void) | undefined;
  readonly onDelete?: (() => void) | undefined;
  /** "Fork from here" (client contract 1.11): a new conversation up to and including this question. */
  readonly onFork?: (() => void) | undefined;
}): ReactNode {
  if (!onCopy && !onEdit && !onDelete && !onFork) return null;
  return (
    <ReplyActionsRow shown={false} className="actionButtons">
      {onCopy && (
        <ReplyActionButton label={t('features.chatMessages.replyActions.copy', 'Copy to clipboard')} onClick={onCopy}>
          <ContentCopyIcon />
        </ReplyActionButton>
      )}
      {onEdit && (
        <ReplyActionButton label={t('features.chatMessages.replyActions.edit', 'Edit the message and regenerate answer')} onClick={onEdit}>
          <EditOutlinedIcon />
        </ReplyActionButton>
      )}
      {onFork && (
        <ReplyActionButton label={t('features.chatMessages.forkFromHere', 'Fork from here')} data-testid="message-fork" onClick={onFork}>
          <CallSplitOutlinedIcon />
        </ReplyActionButton>
      )}
      {onDelete && (
        <ReplyActionButton label={t('features.chatMessages.replyActions.delete', 'Delete')} onClick={onDelete}>
          <DeleteOutlinedIcon />
        </ReplyActionButton>
      )}
    </ReplyActionsRow>
  );
}
