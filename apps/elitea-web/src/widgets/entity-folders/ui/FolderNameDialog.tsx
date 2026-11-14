import { useState, type ReactNode } from 'react';

import TextField from '@mui/material/TextField';

import { t } from '@/shared/i18n';
import { BaseModal } from '@/shared/ui/BaseModal';

/** The longest folder name the server accepts (pkg/entitylimits). */
const NAME_MAX = 128;

interface FolderNameDialogProps {
  readonly open: boolean;
  /** The folder's current name when renaming; empty when creating. */
  readonly initialName: string;
  readonly title: string;
  readonly confirmText: string;
  /** The server's refusal of the last attempt (a duplicate name), shown inside the dialog. */
  readonly errorMessage: string | undefined;
  readonly busy: boolean;
  readonly onClose: () => void;
  readonly onSubmit: (name: string) => void;
}

/** Asks for a folder's name. Remount (key it) per opening so the field starts from `initialName`. */
export function FolderNameDialog({
  open,
  initialName,
  title,
  confirmText,
  errorMessage,
  busy,
  onClose,
  onSubmit,
}: FolderNameDialogProps): ReactNode {
  const [name, setName] = useState(initialName);
  const trimmed = name.trim();
  const submit = (): void => {
    if (trimmed !== '' && !busy) onSubmit(trimmed);
  };
  return (
    <BaseModal
      open={open}
      title={title}
      onClose={onClose}
      onConfirm={submit}
      data-testid="entity-folder-name-dialog"
      actions={{ confirmText, confirming: busy || trimmed === '' }}
      content={
        <TextField
          fullWidth
          value={name}
          label={t('widgets.entityFolders.nameLabel', 'Folder name')}
          error={errorMessage !== undefined}
          helperText={errorMessage ?? ' '}
          slotProps={{
            htmlInput: { maxLength: NAME_MAX, 'data-testid': 'entity-folder-name-input' },
          }}
          onChange={(event) => {
            setName(event.target.value);
          }}
          onKeyDown={(event) => {
            if (event.key === 'Enter') submit();
          }}
        />
      }
    />
  );
}
