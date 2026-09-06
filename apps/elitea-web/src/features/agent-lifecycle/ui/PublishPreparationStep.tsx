import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import FormControlLabel from '@mui/material/FormControlLabel';
import MenuItem from '@mui/material/MenuItem';
import Select from '@mui/material/Select';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';

import { PUBLISH_CATEGORIES, type PublishCategory } from '../lib/publishValidation';
import { getPublishingTerms } from '../lib/publishingTerms';

/**
 * Step 1 of the publish wizard: the version name, the category and the terms.
 *
 * Split out of `PublishVersionDialog` because the dialog holds two steps and
 * both of them inline put it over the §3.5 complexity budget. The split is
 * along the seam the wizard already has, so nothing new has to be kept in
 * step between them.
 */
export interface PublishPreparationStepProps {
  readonly versionName: string;
  readonly onVersionNameChange: (next: string) => void;
  readonly nameValid: boolean;
  readonly category: PublishCategory | '';
  readonly onCategoryChange: (next: PublishCategory | '') => void;
  readonly agreed: boolean;
  readonly onAgreedChange: (next: boolean) => void;
}

export function PublishPreparationStep({
  versionName,
  onVersionNameChange,
  nameValid,
  category,
  onCategoryChange,
  agreed,
  onAgreedChange,
}: PublishPreparationStepProps): ReactNode {
  return (
    <>
      <TextField
        label={t('features.agentLifecycle.publish.versionName', 'Version name')}
        value={versionName}
        onChange={(event) => onVersionNameChange(event.target.value)}
        error={versionName !== '' && !nameValid}
        helperText={t(
          'features.agentLifecycle.publish.versionNameHelp',
          'Only letters, numbers, dots, hyphens and underscores are allowed.',
        )}
        slotProps={{ htmlInput: { 'data-testid': 'publish-version-name' } }}
        fullWidth
      />
      <Select
        displayEmpty
        value={category}
        onChange={(event) => onCategoryChange(event.target.value)}
        data-testid="publish-category"
        fullWidth
      >
        <MenuItem value="">{t('features.agentLifecycle.publish.noCategory', 'Category')}</MenuItem>
        {PUBLISH_CATEGORIES.map((name) => (
          <MenuItem
            key={name}
            value={name}
          >
            {name}
          </MenuItem>
        ))}
      </Select>
      <Typography variant="labelMedium">{t('features.agentLifecycle.publish.terms', 'Publishing Terms')}</Typography>
      <Box sx={termsSx}>
        {getPublishingTerms().map((term) => (
          <Box key={term.heading}>
            <Typography variant="labelSmall">{term.heading}</Typography>
            {term.lines.map((line) => (
              <Typography
                key={line}
                variant="bodySmall"
              >
                {line}
              </Typography>
            ))}
          </Box>
        ))}
      </Box>
      <FormControlLabel
        control={
          <BaseCheckbox
            checked={agreed}
            onChange={(_event, checked) => onAgreedChange(checked)}
            data-testid="publish-terms-agree"
          />
        }
        label={t('features.agentLifecycle.publish.agree', 'I agree with the Publishing Terms.')}
      />
    </>
  );
}

const termsSx: SxProps<Theme> = {
  maxHeight: '10rem',
  overflowY: 'auto',
  display: 'flex',
  flexDirection: 'column',
  gap: '0.5rem',
};
