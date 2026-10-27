/**
 * The action row under a question (`UserMessage`), drawn as the reply's row
 * (`ApplicationAnswerActions`): one compact row OUTSIDE the bubble, quiet
 * 16px outlined icons (`ReplyActionButton`), Copy · Edit · Fork · Delete,
 * then the muted time (`MessageMeta`). Each control renders only when its
 * handler is supplied. Right-aligned under the reader's own bubble,
 * left-aligned under someone else's.
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

import { MessageMeta } from './MessageMeta';
import { ReplyActionButton, ReplyActionsRow } from './ReplyActionButton';

export function UserMessageActions({
  onCopy,
  onEdit,
  onDelete,
  onFork,
  isOwn,
  createdAt,
}: {
  readonly onCopy?: (() => void) | undefined;
  readonly onEdit?: (() => void) | undefined;
  readonly onDelete?: (() => void) | undefined;
  /** "Fork from here" (client contract 1.11): a new conversation up to and including this question. */
  readonly onFork?: (() => void) | undefined;
  /** The reader's own question: the row sits under the right-hand bubble. */
  readonly isOwn: boolean;
  readonly createdAt?: string | undefined;
}): ReactNode {
  if (!onCopy && !onEdit && !onDelete && !onFork && !createdAt) return null;
  return (
    <ReplyActionsRow shown={false} align={isOwn ? 'end' : 'start'} className="actionButtons">
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
      <MessageMeta createdAt={createdAt} />
    </ReplyActionsRow>
  );
}
