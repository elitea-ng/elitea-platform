/**
 * Port of `apps/elitea-ui/src/[fsd]/features/chat/ui/chat-hitl-actions/
 * BlockWithCommentControl.jsx` — the inline "Block with Comment" control of a
 * HITL authorization card.
 *
 * Parallel fan-out can surface several cards at once, so the note is not
 * collected through the shared chat input: the button expands an input ON the
 * card, and each card owns its own state so notes never bleed between cards.
 */
import type { KeyboardEvent, ReactNode } from 'react';
import { useEffect, useRef, useState } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';
import TextField from '@mui/material/TextField';

import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { RejectIcon } from '@/shared/ui/icons/reject-icon';

// Keep the inline note bounded — the runtime guard also caps it server-side.
const MAX_COMMENT_LENGTH = 2000;

/** @public Props for `BlockWithCommentControl`. */
export interface BlockWithCommentControlProps {
  /** Called with the comment as typed (not trimmed); only offered once it has non-blank text. */
  readonly onSubmit: (comment: string) => void;
  readonly disabled?: boolean;
  readonly triggerTestId?: string | undefined;
  readonly textareaTestId?: string | undefined;
  readonly submitTestId?: string | undefined;
}

/** `BlockWithCommentControl` — a "Block with Comment" button that expands into a note box. */
export function BlockWithCommentControl({
  onSubmit,
  disabled = false,
  triggerTestId,
  textareaTestId,
  submitTestId,
}: BlockWithCommentControlProps): ReactNode {
  const [open, setOpen] = useState(false);
  const [comment, setComment] = useState('');
  const label = t('features.chatMessages.hitl.blockWithComment', 'Block with Comment');

  const inputRef = useRef<HTMLTextAreaElement>(null);

  // Focus the box as it opens (the baseline's `autoFocus`, without the attribute).
  useEffect(() => {
    if (open) inputRef.current?.focus();
  }, [open]);

  // Another action can decide this card while the box is open: collapse it so
  // the disabled textarea cannot get stuck on the card.
  useEffect(() => {
    if (disabled) {
      setOpen(false);
      setComment('');
    }
  }, [disabled]);

  const handleCancel = (): void => {
    setOpen(false);
    setComment('');
  };

  const handleSubmit = (): void => {
    if (!comment.trim()) return;
    onSubmit(comment);
    handleCancel();
  };

  const handleKeyDown = (event: KeyboardEvent): void => {
    if (event.key === 'Enter' && (event.metaKey || event.ctrlKey)) {
      event.preventDefault();
      handleSubmit();
    } else if (event.key === 'Escape') {
      event.preventDefault();
      handleCancel();
    }
  };

  if (!open) {
    return (
      <BaseBtn
        data-testid={triggerTestId}
        variant="secondary"
        startIcon={<RejectIcon />}
        onClick={() => setOpen(true)}
        disabled={disabled}
      >
        {label}
      </BaseBtn>
    );
  }

  return (
    <Box sx={inlineBoxSx}>
      <TextField
        multiline
        minRows={2}
        maxRows={6}
        inputRef={inputRef}
        fullWidth
        size="small"
        value={comment}
        onChange={(event) => setComment(event.target.value.slice(0, MAX_COMMENT_LENGTH))}
        onKeyDown={handleKeyDown}
        disabled={disabled}
        placeholder={t(
          'features.chatMessages.hitl.blockCommentPlaceholder',
          'Tell the agent why this call is blocked and what to do instead. Press ⌘/Ctrl+Enter to send.',
        )}
        slotProps={{
          input: { sx: inlineInputSx },
          htmlInput: { maxLength: MAX_COMMENT_LENGTH, 'data-testid': textareaTestId },
        }}
      />
      <Box sx={inlineActionsSx}>
        <BaseBtn
          variant="tertiary"
          onClick={handleCancel}
          disabled={disabled}
        >
          {t('features.chatMessages.hitl.cancel', 'Cancel')}
        </BaseBtn>
        <BaseBtn
          data-testid={submitTestId}
          variant="alarm"
          startIcon={<RejectIcon />}
          onClick={handleSubmit}
          disabled={disabled || !comment.trim()}
        >
          {label}
        </BaseBtn>
      </Box>
    </Box>
  );
}

/** @public Shared with `EditControl` — the same inline box on the same card. */
export const inlineBoxSx: SxProps<Theme> = {
  width: '100%',
  display: 'flex',
  flexDirection: 'column',
  gap: '0.5rem',
  marginTop: '0.125rem',
};

/** @public Shared with `EditControl`. */
export const inlineInputSx: SxProps<Theme> = (theme) => ({
  padding: '0.5rem 0.625rem',
  backgroundColor: theme.vars.palette.background.secondary,
  color: theme.vars.palette.text.primary,
});

/** @public Shared with `EditControl`. */
export const inlineActionsSx: SxProps<Theme> = {
  display: 'flex',
  gap: '0.5rem',
  alignItems: 'center',
  justifyContent: 'flex-end',
};
