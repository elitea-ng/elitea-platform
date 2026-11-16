import type { ReactNode } from 'react';
import { useEffect, useState } from 'react';

import TextField from '@mui/material/TextField';

import { t } from '@/shared/i18n';
import { BaseModal } from '@/shared/ui/BaseModal';

import { isDuplicateVersionName, nextVersionName } from '../lib/skillVersionName';

export interface NewSkillVersionModalProps {
  readonly open: boolean;
  /** Every version name the skill already has. */
  readonly existingNames: readonly string[];
  readonly confirming: boolean;
  /** A refusal from the server (e.g. a name taken in a concurrent edit). */
  readonly error?: string | undefined;
  readonly onClose: () => void;
  readonly onConfirm: (name: string) => void;
}

function nameProblem(name: string, existingNames: readonly string[]): string | undefined {
  if (isDuplicateVersionName(name, existingNames)) {
    return t('skills.edit.versionNameExists', 'Version name already exists. Please choose a different name.');
  }
  if (name.toLowerCase() === 'base') {
    return t('skills.edit.versionNameReserved', '"base" is reserved. Please choose a different name.');
  }
  return undefined;
}

/**
 * "Save as new version". The name opens pre-filled with the next free number
 * (#2102), and a name the skill already has is refused in the dialog with the
 * Save button disabled (#2103) rather than silently doing nothing.
 */
export function NewSkillVersionModal({
  open,
  existingNames,
  confirming,
  error,
  onClose,
  onConfirm,
}: NewSkillVersionModalProps): ReactNode {
  const [name, setName] = useState('');
  const [touched, setTouched] = useState(false);
  const [serverErrorShown, setServerErrorShown] = useState(true);
  const namesKey = existingNames.join('\u0000');

  useEffect(() => {
    // A fresh dialog starts untouched, with no leftover refusal from the last attempt.
    if (open) {
      setTouched(false);
      setServerErrorShown(true);
    }
  }, [open]);

  useEffect(() => {
    // Seeded on open and again when the versions list arrives or changes, until
    // the user types: their edit is never overwritten, and a name taken by the
    // list that was still loading is not offered.
    if (open && !touched) setName(nextVersionName(existingNames));
    // oxlint-disable-next-line react/exhaustive-deps -- namesKey stands for existingNames.
  }, [open, touched, namesKey]);

  useEffect(() => {
    // A new refusal from the server shows again, even after the name was edited.
    if (error !== undefined) setServerErrorShown(true);
  }, [error]);

  const trimmed = name.trim();
  const problem = nameProblem(trimmed, existingNames);
  const helper = problem ?? (serverErrorShown ? error : undefined);
  const invalid = trimmed === '' || problem !== undefined;

  return (
    <BaseModal
      open={open}
      title={t('skills.edit.newVersionTitle', 'Create skill version')}
      onClose={onClose}
      onConfirm={() => {
        if (!invalid) onConfirm(trimmed);
      }}
      actions={{ confirming, confirmDisabled: invalid }}
      content={
        <TextField
          fullWidth
          autoFocus
          label={t('skills.edit.versionName', 'Version name')}
          value={name}
          onChange={(event) => {
            setName(event.target.value);
            setTouched(true);
            // The refusal belonged to the old name.
            setServerErrorShown(false);
          }}
          error={helper !== undefined}
          helperText={helper}
          slotProps={{ htmlInput: { 'data-testid': 'skill-version-name-input' } }}
        />
      }
    />
  );
}
