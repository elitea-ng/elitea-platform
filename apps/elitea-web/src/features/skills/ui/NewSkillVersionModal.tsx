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

  useEffect(() => {
    // Re-seeded only when the dialog opens: a refetch while the user types must not overwrite their edit.
    if (open) setName(nextVersionName(existingNames));
    // oxlint-disable-next-line react/exhaustive-deps -- seeded on open only.
  }, [open]);

  const trimmed = name.trim();
  const problem = nameProblem(trimmed, existingNames);
  const helper = problem ?? error;
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
          onChange={(event) => setName(event.target.value)}
          error={helper !== undefined}
          helperText={helper}
          slotProps={{ htmlInput: { 'data-testid': 'skill-version-name-input' } }}
        />
      }
    />
  );
}
