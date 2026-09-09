/**
 * LongTermMemoryFormDialog — create (or edit) one persistent, cross-
 * conversation memory (#870).
 *
 * `source_conversation_id` is never edited here — it is stamped only by the
 * "Remember this" chat message action (`RememberMemoryAction.tsx`) at
 * creation time, and preserved as-is on every later edit through this
 * dialog (see `LongTermMemoryManagement.tsx`'s `submitForm`).
 */
import { useCallback, useEffect, useState } from 'react';

import Box from '@mui/material/Box';
import FormControlLabel from '@mui/material/FormControlLabel';
import Switch from '@mui/material/Switch';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BaseModal } from '@/shared/ui/BaseModal';

const CONTENT_MAX_LEN = 4000; // memories.MaxContentBytes (internal/api/v2/memories/handler.go)

export interface LongTermMemoryFormValues {
  readonly content: string;
  readonly tags: string;
  readonly enabled: boolean;
}

export interface LongTermMemoryFormDialogProps {
  readonly open: boolean;
  readonly isSaving: boolean;
  /** Present when editing an existing memory; absent for create. */
  readonly initialValues?: LongTermMemoryFormValues | undefined;
  /** The server's own refusal reason for the last submit, if it failed. */
  readonly serverError?: string | undefined;
  readonly onClose: () => void;
  readonly onSubmit: (values: LongTermMemoryFormValues) => void;
}

const contentSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '1.25rem', minWidth: '25rem' };
const descriptionSx: SxProps<Theme> = { color: 'text.secondary' };

export function LongTermMemoryFormDialog({
  open,
  isSaving,
  initialValues,
  serverError,
  onClose,
  onSubmit,
}: LongTermMemoryFormDialogProps) {
  const [content, setContent] = useState('');
  const [tags, setTags] = useState('');
  const [enabled, setEnabled] = useState(true);
  const [contentError, setContentError] = useState('');
  const [serverErrorDismissed, setServerErrorDismissed] = useState(false);

  useEffect(() => {
    if (!open) return;
    setContent(initialValues?.content ?? '');
    setTags(initialValues?.tags ?? '');
    setEnabled(initialValues?.enabled ?? true);
    setContentError('');
    setServerErrorDismissed(false);
  }, [open, initialValues]);

  useEffect(() => {
    setServerErrorDismissed(false);
  }, [serverError]);

  const handleSubmit = useCallback(() => {
    const trimmed = content.trim();
    if (!trimmed) {
      setContentError(t('settings.longTermMemory.form.contentRequired', 'A memory needs some text'));
      return;
    }
    if (trimmed.length > CONTENT_MAX_LEN) {
      setContentError(
        t('settings.longTermMemory.form.contentTooLong', 'That memory is too long (max {{max}} characters)', { max: CONTENT_MAX_LEN }),
      );
      return;
    }
    onSubmit({ content: trimmed, tags, enabled });
  }, [content, tags, enabled, onSubmit]);

  const displayedContentError = contentError || (!serverErrorDismissed && serverError ? serverError : '');
  const isEdit = initialValues !== undefined;

  return (
    <BaseModal
      open={open}
      onClose={onClose}
      title={isEdit ? t('settings.longTermMemory.form.editTitle', 'Edit memory') : t('settings.longTermMemory.form.createTitle', 'New memory')}
      content={
        <Box sx={contentSx}>
          <Typography variant="bodyMedium" sx={descriptionSx}>
            {t(
              'settings.longTermMemory.form.description',
              'A fact Elitea should remember about you in every conversation, not just this one.',
            )}
          </Typography>
          <TextField
            label={t('settings.longTermMemory.form.contentLabel', 'Memory *')}
            placeholder={t('settings.longTermMemory.form.contentPlaceholder', 'e.g. Prefers TypeScript over Python for new services')}
            value={content}
            onChange={(event) => {
              setContent(event.target.value);
              setContentError('');
              setServerErrorDismissed(true);
            }}
            error={displayedContentError !== ''}
            helperText={displayedContentError || undefined}
            multiline
            minRows={3}
            maxRows={8}
            fullWidth
            data-testid="long-term-memory-form-content"
          />
          <TextField
            label={t('settings.longTermMemory.form.tagsLabel', 'Tags')}
            placeholder={t('settings.longTermMemory.form.tagsPlaceholder', 'Comma-separated, e.g. preferences, engineering')}
            value={tags}
            onChange={(event) => setTags(event.target.value)}
            fullWidth
            data-testid="long-term-memory-form-tags"
          />
          <FormControlLabel
            control={
              <Switch checked={enabled} onChange={(event) => setEnabled(event.target.checked)} data-testid="long-term-memory-form-enabled" />
            }
            label={t('settings.longTermMemory.form.enabledLabel', 'Enabled')}
          />
        </Box>
      }
      actions={{
        confirming: isSaving,
        confirmText: isEdit ? t('settings.longTermMemory.form.save', 'Save') : t('settings.longTermMemory.form.create', 'Create'),
        cancelText: t('settings.longTermMemory.form.cancel', 'Cancel'),
      }}
      onConfirm={handleSubmit}
      data-testid="long-term-memory-form-dialog"
    />
  );
}
