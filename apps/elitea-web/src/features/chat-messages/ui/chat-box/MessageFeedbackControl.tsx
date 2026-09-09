/**
 * Thumbs up / thumbs down + optional comment on an assistant message (#880).
 *
 * A self-contained control — it owns its own read (`conversationApi.
 * useMessageFeedback`) and its own writes (`useSetMessageFeedback`/
 * `useDeleteMessageFeedback`) rather than threading feedback state through
 * `ApplicationAnswer`'s already-large prop surface, matching how `entities/
 * conversation` API calls are consumed elsewhere in this tree (e.g.
 * `MessageAttachmentList`'s per-attachment data fetching).
 *
 * VOTING IS A TOGGLE: clicking the currently-selected thumb RETRACTS the
 * vote (DELETE); clicking the other thumb REPLACES it (the server's own
 * upsert — see `messageFeedbackApi.ts`). A comment rides on whichever vote
 * is active; the comment control only appears once a vote exists, since a
 * comment with no rating has nothing to attach to (the server's
 * `MessageFeedbackRequest.rating` is required).
 *
 * The aggregate counts are the Tooltip title on each thumb — "shown on
 * hover" per the issue's own wording — and the caller's own vote is the
 * highlighted (filled) icon + `aria-pressed`.
 */
import type { MouseEvent, ReactNode } from 'react';
import { useCallback, useState } from 'react';

import ModeCommentOutlinedIcon from '@mui/icons-material/ModeCommentOutlined';
import ThumbDownIcon from '@mui/icons-material/ThumbDown';
import ThumbDownOutlinedIcon from '@mui/icons-material/ThumbDownOutlined';
import ThumbUpIcon from '@mui/icons-material/ThumbUp';
import ThumbUpOutlinedIcon from '@mui/icons-material/ThumbUpOutlined';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import IconButton from '@mui/material/IconButton';
import Popover from '@mui/material/Popover';
import TextField from '@mui/material/TextField';
import Tooltip from '@mui/material/Tooltip';

import { conversationApi } from '@/entities/conversation';
import { t } from '@/shared/i18n';

/** Matches `messageFeedbackCommentMaxLen` (internal/api/v2/conversations/handler.go). */
const COMMENT_MAX_LEN = 2000;

export interface MessageFeedbackControlProps {
  readonly projectId: string;
  readonly messageId: string;
  readonly disabled?: boolean;
}

export function MessageFeedbackControl({
  projectId,
  messageId,
  disabled = false,
}: MessageFeedbackControlProps): ReactNode {
  const target = { projectId, messageId };
  const { data: summary } = conversationApi.useMessageFeedback(disabled ? undefined : target);
  const setFeedback = conversationApi.useSetMessageFeedback();
  const deleteFeedback = conversationApi.useDeleteMessageFeedback();
  const [commentAnchor, setCommentAnchor] = useState<HTMLElement | null>(null);
  const [commentDraft, setCommentDraft] = useState('');

  const mine = summary?.mine;
  const isBusy = setFeedback.isPending || deleteFeedback.isPending;
  const isDisabled = disabled || isBusy;

  const handleVote = useCallback(
    (rating: 1 | -1) => {
      if (mine?.rating === rating) {
        deleteFeedback.mutate(target);
        return;
      }
      setFeedback.mutate({ ...target, rating, ...(mine?.comment ? { comment: mine.comment } : {}) });
    },
    // eslint-disable-next-line react-hooks/exhaustive-deps -- `target` is a fresh object literal every render; projectId/messageId are its real identity.
    [mine, projectId, messageId, setFeedback, deleteFeedback],
  );

  const handleOpenComment = useCallback(
    (event: MouseEvent<HTMLElement>) => {
      setCommentDraft(mine?.comment ?? '');
      setCommentAnchor(event.currentTarget);
    },
    [mine],
  );

  const handleCloseComment = useCallback(() => setCommentAnchor(null), []);

  const handleSubmitComment = useCallback(() => {
    if (!mine) return; // Defensive: the trigger only renders once `mine` exists.
    setFeedback.mutate({ ...target, rating: mine.rating as 1 | -1, comment: commentDraft });
    setCommentAnchor(null);
    // eslint-disable-next-line react-hooks/exhaustive-deps -- see handleVote
  }, [commentDraft, mine, projectId, messageId, setFeedback]);

  const likeLabel = t('features.chatMessages.feedback.like', 'Like this answer');
  const dislikeLabel = t('features.chatMessages.feedback.dislike', 'Dislike this answer');
  const likeCount = summary
    ? t('features.chatMessages.feedback.likeCount', 'Likes: {{count}}', { count: summary.likes })
    : '';
  const dislikeCount = summary
    ? t('features.chatMessages.feedback.dislikeCount', 'Dislikes: {{count}}', { count: summary.dislikes })
    : '';

  return (
    <Box sx={{ display: 'flex', alignItems: 'center', gap: 0.25 }}>
      <Tooltip title={likeCount} placement="top">
        <span>
          <IconButton
            size="small"
            color={mine?.rating === 1 ? 'primary' : 'tertiary'}
            disabled={isDisabled}
            onClick={() => handleVote(1)}
            aria-label={likeLabel}
            aria-pressed={mine?.rating === 1}
          >
            {mine?.rating === 1 ? <ThumbUpIcon fontSize="small" /> : <ThumbUpOutlinedIcon fontSize="small" />}
          </IconButton>
        </span>
      </Tooltip>
      <Tooltip title={dislikeCount} placement="top">
        <span>
          <IconButton
            size="small"
            color={mine?.rating === -1 ? 'primary' : 'tertiary'}
            disabled={isDisabled}
            onClick={() => handleVote(-1)}
            aria-label={dislikeLabel}
            aria-pressed={mine?.rating === -1}
          >
            {mine?.rating === -1 ? <ThumbDownIcon fontSize="small" /> : <ThumbDownOutlinedIcon fontSize="small" />}
          </IconButton>
        </span>
      </Tooltip>
      {mine && (
        <Tooltip
          title={
            mine.comment
              ? t('features.chatMessages.feedback.editComment', 'Edit your comment')
              : t('features.chatMessages.feedback.addComment', 'Add a comment')
          }
          placement="top"
        >
          <IconButton size="small" color="tertiary" disabled={isDisabled} onClick={handleOpenComment} aria-label={t('features.chatMessages.feedback.addComment', 'Add a comment')}>
            <ModeCommentOutlinedIcon fontSize="small" />
          </IconButton>
        </Tooltip>
      )}
      <Popover
        open={Boolean(commentAnchor)}
        anchorEl={commentAnchor}
        onClose={handleCloseComment}
        anchorOrigin={{ vertical: 'top', horizontal: 'center' }}
        transformOrigin={{ vertical: 'bottom', horizontal: 'center' }}
      >
        <Box sx={{ p: 1.5, display: 'flex', flexDirection: 'column', gap: 1, width: '16rem' }}>
          <TextField
            multiline
            minRows={2}
            maxRows={5}
            size="small"
            value={commentDraft}
            onChange={(event) => setCommentDraft(event.target.value)}
            placeholder={t('features.chatMessages.feedback.commentPlaceholder', 'What could be better? (optional)')}
            slotProps={{ htmlInput: { maxLength: COMMENT_MAX_LEN } }}
          />
          <Button size="small" variant="contained" onClick={handleSubmitComment} disabled={isBusy}>
            {t('features.chatMessages.feedback.saveComment', 'Save')}
          </Button>
        </Box>
      </Popover>
    </Box>
  );
}
