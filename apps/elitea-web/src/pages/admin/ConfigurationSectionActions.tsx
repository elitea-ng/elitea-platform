/**
 * Discard and Save under a Configuration section's form. Save goes through
 * the catalogue mode guard: the `catalogue` section warns before switching
 * to approved_only while connectors in use have no approved request (#1501).
 * A save of the Maintenance section asks for a reason in the one
 * confirmation dialog first (ADR-0059 R3.8b, issue 2614): turning the mode
 * on is a dangerous action, and the server, which cannot tell that from the
 * request alone, asks a reason of every save of the section. Every other
 * section saves at once.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';

import { t } from '@/shared/i18n';

import { useCatalogueModeGuard } from './CatalogueModeGuard';
import { MAINTENANCE_ON_ACTION } from './api/adminConfigurationApi';
import { useAdminDangerousConfirm } from './useAdminDangerousConfirm';
import type { AdminConfigurationPageState } from './useAdminConfigurationPage';

export function ConfigurationSectionActions({ state }: { readonly state: AdminConfigurationPageState }): ReactNode {
  const dangerous = useAdminDangerousConfirm();
  const save = (): void => {
    if (state.maintenanceSave === undefined) {
      state.onSave();
      return;
    }
    const turnsOn = state.maintenanceSave === 'on';
    dangerous.ask({
      action: MAINTENANCE_ON_ACTION,
      title: turnsOn
        ? t('pages.admin.configuration.maintenanceOn.title', 'Turn maintenance mode on')
        : t('pages.admin.configuration.maintenanceSave.title', 'Save the maintenance settings'),
      change: turnsOn
        ? t(
            'pages.admin.configuration.maintenanceOn.change',
            'Every request from a caller without administration access is answered with the maintenance page until the mode is turned off.',
          )
        : t(
            'pages.admin.configuration.maintenanceSave.change',
            'The maintenance page and its switch change for everyone. Every save of this section is recorded with its reason.',
          ),
      reach: { count: undefined, unit: 'people' },
      target: 'maintenance',
      confirmLabel: turnsOn
        ? t('pages.admin.configuration.maintenanceOn.confirm', 'Turn on')
        : t('pages.admin.configuration.save', 'Save'),
      run: (reason) => state.onSave(reason),
    });
  };
  const guard = useCatalogueModeGuard(state.activeSection?.id, state.values, state.isDirty, save);
  const idle = state.readOnly || !state.isDirty || state.isSaving;
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
      {dangerous.dialog}
    </>
  );
}
