/**
 * Create / Edit dialog for one global secret (unit A14).
 *
 * One component for both, because the reference's two dialogs
 * (`CreateSecretDialog.jsx`, `EditSecretDialog.jsx`) differ in exactly two
 * things: whether the name field is editable, and whether the name is checked
 * against the existing set. Splitting them here would duplicate the value
 * field, the validation, the in-flight state and the error surface.
 *
 * ## The value is never pre-filled on edit
 *
 * The reference's Edit dialog opens with an empty value field and the name
 * disabled, and this keeps that. Pre-filling would mean fetching the plaintext
 * to display it — putting a credential on screen because a dialog opened, rather
 * than because the operator asked to see it.
 *
 * ## Client-side validation is a courtesy, not a gate
 *
 * The name check and the duplicate check exist so the operator gets an answer
 * without a round trip. The SERVER refuses both independently
 * (`internal/api/v2/secrets/admin.go` — `validSecretName`, and the create path's
 * "already exists" 400), and its refusal is what this dialog renders when the
 * two disagree.
 */
import { useEffect, useState } from 'react';

import Alert from '@mui/material/Alert';
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import TextField from '@mui/material/TextField';

import { t } from '@/shared/i18n';

/** Matches the server's `validSecretName` and the legacy `{{secret.…}}` pattern. */
const SECRET_NAME_PATTERN = /^[A-Za-z0-9_]+$/;

export interface AdminSecretDialogProps {
  readonly open: boolean;
  /** `undefined` ⇒ create; a name ⇒ edit that secret's value. */
  readonly editingName: string | undefined;
  /** Every name currently in the vault, for the duplicate check on create. */
  readonly existingNames: ReadonlySet<string>;
  readonly isSaving: boolean;
  /** The server's own words when the last attempt was refused. */
  readonly serverError: string | undefined;
  readonly onClose: () => void;
  readonly onSubmit: (name: string, value: string) => void;
}

export function AdminSecretDialog({
  open,
  editingName,
  existingNames,
  isSaving,
  serverError,
  onClose,
  onSubmit,
}: AdminSecretDialogProps) {
  const isEdit = editingName !== undefined;
  const [name, setName] = useState('');
  const [value, setValue] = useState('');
  const [localError, setLocalError] = useState('');

  useEffect(() => {
    if (!open) return;
    setName(editingName ?? '');
    setValue('');
    setLocalError('');
  }, [open, editingName]);

  const handleSubmit = (): void => {
    const trimmed = name.trim();
    if (trimmed === '') {
      setLocalError(t('pages.admin.secrets.dialog.error.nameRequired', 'Secret name is required.'));
      return;
    }
    if (!SECRET_NAME_PATTERN.test(trimmed)) {
      setLocalError(
        t(
          'pages.admin.secrets.dialog.error.nameFormat',
          'Name must contain only letters, digits and underscores.',
        ),
      );
      return;
    }
    if (!isEdit && existingNames.has(trimmed)) {
      setLocalError(
        t('pages.admin.secrets.dialog.error.duplicate', 'A secret with that name already exists.'),
      );
      return;
    }
    if (value === '') {
      setLocalError(t('pages.admin.secrets.dialog.error.valueRequired', 'Secret value is required.'));
      return;
    }
    setLocalError('');
    onSubmit(trimmed, value);
  };

  const error = localError !== '' ? localError : serverError;

  return (
    <Dialog open={open} onClose={onClose} maxWidth="sm" fullWidth data-testid="admin-secret-dialog">
      <DialogTitle>
        {isEdit
          ? t('pages.admin.secrets.dialog.editTitle', 'Edit secret')
          : t('pages.admin.secrets.dialog.createTitle', 'Create secret')}
      </DialogTitle>
      <DialogContent>
        {error !== undefined && error !== '' ? (
          <Alert severity="error" sx={{ marginBottom: '1rem' }}>
            {error}
          </Alert>
        ) : null}
        <TextField
          margin="dense"
          fullWidth
          label={t('pages.admin.secrets.dialog.name', 'Secret name')}
          value={name}
          disabled={isEdit || isSaving}
          onChange={(event) => setName(event.target.value)}
          helperText={t(
            'pages.admin.secrets.dialog.nameHelp',
            'Letters, digits and underscores only',
          )}
        />
        {/*
          No `autoFocus`: jsx-a11y(no-autofocus) forbids it, and the reference
          page's Edit dialog only used it to skip the disabled name field —
          which the browser skips on its own.
        */}
        <TextField
          margin="dense"
          fullWidth
          multiline
          minRows={2}
          maxRows={6}
          label={
            isEdit
              ? t('pages.admin.secrets.dialog.newValue', 'New value')
              : t('pages.admin.secrets.dialog.value', 'Secret value')
          }
          value={value}
          disabled={isSaving}
          onChange={(event) => setValue(event.target.value)}
        />
      </DialogContent>
      <DialogActions sx={{ padding: '0 1.5rem 1rem' }}>
        <Button variant="text" disabled={isSaving} onClick={onClose}>
          {t('pages.admin.secrets.dialog.cancel', 'Cancel')}
        </Button>
        <Button variant="contained" disabled={isSaving} onClick={handleSubmit}>
          {isEdit
            ? t('pages.admin.secrets.dialog.save', 'Save')
            : t('pages.admin.secrets.dialog.create', 'Create')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}
