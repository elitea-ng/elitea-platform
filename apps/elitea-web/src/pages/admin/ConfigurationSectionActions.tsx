/**
 * Discard and Save under a Configuration section's form. Save goes through
 * the catalogue mode guard: the `catalogue` section warns before switching
 * to approved_only while connectors in use have no approved request (#1501);
 * every other section saves at once.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';

import { t } from '@/shared/i18n';

import { useCatalogueModeGuard } from './CatalogueModeGuard';
import type { AdminConfigurationPageState } from './useAdminConfigurationPage';

export function ConfigurationSectionActions({ state }: { readonly state: AdminConfigurationPageState }): ReactNode {
  const guard = useCatalogueModeGuard(state.activeSection?.id, state.values, state.isDirty, state.onSave);
  const idle = !state.isDirty || state.isSaving;
  return (
    <>
      {guard.warning}
      <Box sx={{ display: 'flex', gap: '0.5rem' }}>
        <Button size="small" variant="secondary" disabled={idle} onClick={state.onDiscard} sx={{ textTransform: 'none' }}>
          {t('pages.admin.configuration.discard', 'Discard')}
        </Button>
        <Button
          size="small"
          variant="elitea"
          color="primary"
          disabled={idle}
          onClick={guard.onSave}
          sx={{ textTransform: 'none' }}
        >
          {state.isSaving ? t('pages.admin.configuration.saving', 'Saving…') : t('pages.admin.configuration.save', 'Save')}
        </Button>
      </Box>
      {guard.dialog}
    </>
  );
}
