/**
 * Port of `apps/elitea-ui/src/[fsd]/features/chat/ui/chat-hitl-actions/
 * EditControl.jsx` — the inline "Edit" control of a HITL card.
 *
 * The edited message is not collected through the shared chat input (which
 * carries no tool call id to match the right card): the button expands an
 * input ON the card, and each card owns its own state.
 */
import type { KeyboardEvent, ReactNode } from 'react';
import { useEffect, useRef, useState } from 'react';

import Box from '@mui/material/Box';
import TextField from '@mui/material/TextField';

import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { EditIcon } from '@/shared/ui/icons/edit-icon';

import { inlineActionsSx, inlineBoxSx, inlineInputSx } from './BlockWithCommentControl';

// Keep the inline edit bounded — the runtime guard also caps it server-side.
const MAX_EDIT_LENGTH = 2000;

/** @public Props for `EditControl`. */
export interface EditControlProps {
  /** Called with the trimmed edited message. */
  readonly onSubmit: (newValue: string) => void;
  readonly disabled?: boolean;
}

/** `EditControl` — an "Edit" button that expands into an inline edit box. */
export function EditControl({ onSubmit, disabled = false }: EditControlProps): ReactNode {
  const [open, setOpen] = useState(false);
  const [text, setText] = useState('');
  const trimmedText = text.trim();

  const inputRef = useRef<HTMLTextAreaElement>(null);

  // Focus the box as it opens (the baseline's `autoFocus`, without the attribute).
  useEffect(() => {
    if (open) inputRef.current?.focus();
  }, [open]);

  // Another action can decide this card while the box is open: collapse it.
  useEffect(() => {
    if (disabled) {
      setOpen(false);
      setText('');
    }
  }, [disabled]);

  const handleCancel = (): void => {
    setOpen(false);
    setText('');
  };

  const handleSubmit = (): void => {
    if (!trimmedText) return;
    onSubmit(trimmedText);
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
        data-testid="chat-hitl-edit-button"
        variant="neutral"
        startIcon={<EditIcon />}
        onClick={() => setOpen(true)}
        disabled={disabled}
      >
        {t('features.chatMessages.hitl.edit', 'Edit')}
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
        value={text}
        onChange={(event) => setText(event.target.value.slice(0, MAX_EDIT_LENGTH))}
        onKeyDown={handleKeyDown}
        disabled={disabled}
        placeholder={t(
          'features.chatMessages.hitl.editPlaceholder',
          'Enter your edited message to proceed. Press ⌘/Ctrl+Enter to send.',
        )}
        slotProps={{ input: { sx: inlineInputSx }, htmlInput: { maxLength: MAX_EDIT_LENGTH } }}
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
          variant="neutral"
          startIcon={<EditIcon />}
          onClick={handleSubmit}
          disabled={disabled || !trimmedText}
        >
          {t('features.chatMessages.hitl.submitEdit', 'Submit Edit')}
        </BaseBtn>
      </Box>
    </Box>
  );
}
