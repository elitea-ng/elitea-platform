/**
 * A project's Send to cloud switch (ADR-0034 decision 11, owner decision
 * 2026-10-19), as both the admin Projects page and the project settings page
 * show it: the effective state, whether it is the project's own choice or the
 * platform's default for projects, and a three-way choice — use the default,
 * on, off.
 *
 * Presentational: the caller reads and writes the switch (the two pages use
 * two routes with two permission models) and passes the server's state in.
 * The platform switch always wins, so while it is off "On" is not offered,
 * and `lockWhenPlatformOff` disables the whole choice for callers that
 * should not even prepare a value (the project settings page).
 */
import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Chip from '@mui/material/Chip';
import CircularProgress from '@mui/material/CircularProgress';
import Typography from '@mui/material/Typography';
import { memo, useCallback } from 'react';

import type { ProjectCloudExecutorSwitch } from '@/shared/api/generated/model';
import { i18n, t } from '@/shared/i18n';
import { RadioButtonGroup } from '@/shared/ui/RadioButtonGroup';

import catalogue from '../i18n/en.cloudExecutorSwitch.json';

// These strings ride with the chunk of the page that renders the switch, not
// the initial catalogue (bundle budget; scripts/i18n-backfill.mjs,
// ROUTE_CATALOGUES). Deep merge, no overwrite.
i18n.addResourceBundle('en', 'translation', catalogue, true, false);

/** `default` follows the platform's project default; `on` / `off` are the project's own value. */
type CloudExecutorSwitchChoice = 'default' | 'on' | 'off';

function choiceOf(state: Pick<ProjectCloudExecutorSwitch, 'explicit'>): CloudExecutorSwitchChoice {
  if (state.explicit === null || state.explicit === undefined) return 'default';
  return state.explicit ? 'on' : 'off';
}

/** The PUT body for a choice: `null` goes back to the default. */
function enabledOf(choice: CloudExecutorSwitchChoice): boolean | null {
  if (choice === 'default') return null;
  return choice === 'on';
}

export interface CloudExecutorSwitchControlProps {
  /** The server's state; `undefined` while it loads. */
  readonly state: ProjectCloudExecutorSwitch | undefined;
  readonly canEdit: boolean;
  readonly isSaving: boolean;
  /** A load or save failure, already worded for the reader. */
  readonly errorMessage?: string | undefined;
  /** Disable every choice while the platform is off, not only "On". */
  readonly lockWhenPlatformOff?: boolean;
  /** Shown when `canEdit` is false: who can change it. */
  readonly readOnlyHint: string;
  readonly onChange: (enabled: boolean | null) => void;
}

export const CloudExecutorSwitchControl = memo(function CloudExecutorSwitchControl({
  state,
  canEdit,
  isSaving,
  errorMessage,
  lockWhenPlatformOff = false,
  readOnlyHint,
  onChange,
}: CloudExecutorSwitchControlProps) {
  const handleChange = useCallback(
    (value: string) => {
      if (value === 'default' || value === 'on' || value === 'off') onChange(enabledOf(value));
    },
    [onChange],
  );

  if (!state) {
    return errorMessage ? (
      <Alert severity="error">{errorMessage}</Alert>
    ) : (
      <CircularProgress size={20} aria-label={t('features.cloudExecutorSwitch.loading', 'Loading')} />
    );
  }

  const platformOff = !state.platform_enabled;
  const locked = !canEdit || isSaving || (platformOff && lockWhenPlatformOff);

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: '0.75rem' }} data-testid="cloud-executor-switch">
      <SwitchStatus state={state} isSaving={isSaving} />

      {platformOff ? (
        <Alert severity="info">
          {t(
            'features.cloudExecutorSwitch.platformOff',
            'The cloud executor is turned off for this deployment, so Send to cloud is off in every project. An administrator can turn it on in Admin, Configuration, Cloud executor.',
          )}
        </Alert>
      ) : null}

      <RadioButtonGroup
        aria-label={t('features.cloudExecutorSwitch.groupLabel', 'Send to cloud for this project')}
        value={choiceOf(state)}
        onChange={handleChange}
        disabled={locked}
        wrapRow
        items={[
          {
            value: 'default',
            label: t('features.cloudExecutorSwitch.option.default', 'Use the default for projects'),
            description: state.project_default_enabled
              ? t('features.cloudExecutorSwitch.option.defaultOn', 'The default is on')
              : t('features.cloudExecutorSwitch.option.defaultOff', 'The default is off'),
          },
          {
            value: 'on',
            label: t('features.cloudExecutorSwitch.option.on', 'On'),
            disabled: platformOff,
          },
          { value: 'off', label: t('features.cloudExecutorSwitch.option.off', 'Off') },
        ]}
      />

      {!canEdit ? (
        <Typography variant="bodySmall" color="text.secondary">
          {readOnlyHint}
        </Typography>
      ) : null}
      {errorMessage ? <Alert severity="error">{errorMessage}</Alert> : null}
    </Box>
  );
});

/** The effective state and where it comes from. */
function sourceLabelOf(state: ProjectCloudExecutorSwitch): string {
  if (!state.platform_enabled) return t('features.cloudExecutorSwitch.source.platformOff', 'Off for this deployment');
  return state.source === 'explicit'
    ? t('features.cloudExecutorSwitch.source.explicit', 'Set for this project')
    : t('features.cloudExecutorSwitch.source.default', 'Following the default for projects');
}

function SwitchStatus({ state, isSaving }: { readonly state: ProjectCloudExecutorSwitch; readonly isSaving: boolean }) {
  return (
    <Box sx={{ display: 'flex', alignItems: 'center', gap: '0.5rem', flexWrap: 'wrap' }}>
      <Typography variant="bodyMedium">{t('features.cloudExecutorSwitch.status.label', 'Send to cloud')}</Typography>
      <Chip
        size="small"
        variant="outlined"
        color={state.enabled ? 'success' : 'default'}
        label={
          state.enabled
            ? t('features.cloudExecutorSwitch.status.on', 'On')
            : t('features.cloudExecutorSwitch.status.off', 'Off')
        }
        data-testid="cloud-executor-switch-status"
      />
      <Typography variant="bodySmall" color="text.secondary" data-testid="cloud-executor-switch-source">
        {sourceLabelOf(state)}
      </Typography>
      {isSaving ? <CircularProgress size={14} /> : null}
    </Box>
  );
}
