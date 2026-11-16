import type { ReactNode } from 'react';
import { useEffect, useRef, useState } from 'react';

import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import InputAdornment from '@mui/material/InputAdornment';
import TextField from '@mui/material/TextField';

import { t } from '@/shared/i18n';

import { splitFileName, validateFileName } from '../lib/pathValidation';
import type { ArtifactListItem } from '../model/types';

interface RenameArtifactDialogProps {
  /** The file being renamed; the dialog is closed while this is undefined. */
  readonly item: ArtifactListItem | undefined;
  readonly pending: boolean;
  /** The server's refusal (a taken name, a missing file), shown under the field. */
  readonly error?: string | undefined;
  readonly onClose: () => void;
  /** Receives the new bare file name, extension included and unchanged; the folder part of the key is kept by the caller. */
  readonly onSubmit: (name: string) => void;
}

export function RenameArtifactDialog({ item, pending, error, onClose, onSubmit }: RenameArtifactDialogProps): ReactNode {
  // Only the name before the extension is editable: the extension stays (#2119).
  const [stem, setStem] = useState('');
  const nameRef = useRef<HTMLInputElement>(null);
  const extension = splitFileName(item?.name ?? '').extension;
  useEffect(() => {
    setStem(splitFileName(item?.name ?? '').stem);
  }, [item]);
  const trimmed = stem.trim();
  const fullName = `${trimmed}${extension}`;
  // A name that gains or changes an extension is refused here, as Main refuses it (#2119): typing
  // `Makefile.md` for a file with no extension would otherwise reach the server and come back a 400.
  const keepsExtension = splitFileName(fullName).extension === extension;
  const validation =
    trimmed === ''
      ? ''
      : validateFileName(fullName) || (keepsExtension ? '' : t('artifacts.rename.extension', 'The file extension cannot be changed.'));
  const unchanged = fullName === item?.name;
  const disabled = pending || trimmed === '' || validation !== '' || unchanged;
  const submit = (): void => {
    if (!disabled) onSubmit(fullName);
  };
  return (
    <Dialog
      open={item !== undefined}
      onClose={onClose}
      fullWidth
      maxWidth="sm"
      // Focus the name field once the dialog has opened (instead of `autoFocus`, which jsx-a11y refuses).
      slotProps={{ transition: { onEntered: () => nameRef.current?.focus() } }}
    >
      <DialogTitle>{t('artifacts.rename.title', 'Rename file')}</DialogTitle>
      <DialogContent>
        <TextField
          inputRef={nameRef}
          fullWidth
          margin="dense"
          label={t('artifacts.rename.label', 'File name')}
          value={stem}
          error={validation !== '' || error !== undefined}
          helperText={validation || error || ' '}
          onChange={(event) => setStem(event.target.value)}
          slotProps={{
            input: {
              endAdornment:
                extension === '' ? undefined : (
                  <InputAdornment
                    position="end"
                    data-testid="rename-extension"
                  >
                    {extension}
                  </InputAdornment>
                ),
            },
          }}
          onKeyDown={(event) => {
            if (event.key === 'Enter') {
              event.preventDefault();
              submit();
            }
          }}
        />
      </DialogContent>
      <DialogActions>
        <Button onClick={onClose}>{t('common.cancel', 'Cancel')}</Button>
        <Button
          variant="contained"
          disabled={disabled}
          onClick={submit}
        >
          {t('artifacts.rename.confirm', 'Rename')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}
