/**
 * Admin › Configuration › LLM Proxy — the platform default model (#6826).
 *
 * New projects start with this model. A project with no usable default of its
 * own falls back to it. The choices are the platform models offered to every
 * project: a narrower grant cannot be a new project's default, because a new
 * project is in no project list.
 *
 * A stored model that is no longer offered (deleted, disabled, narrowed) is
 * reported, and the admin is asked to choose again — the acceptance criterion
 * that a missing default must not fail silently.
 */
import type { ReactNode } from 'react';
import { useState } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import MenuItem from '@mui/material/MenuItem';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import { platformDefaultImpact } from './platformDefaultImpact';
import { configFailureReason } from './api/adminConfigurationApi';
import {
  useAdminPlatformDefaultModel,
  usePlatformModelDefaultUsage,
  useSavePlatformDefaultModel,
  type PlatformDefaultModel,
} from './api/adminPlatformDefaultModelApi';

/** The select value: the chosen name, or the stored one until the admin picks. */
export function platformDefaultSelection(view: PlatformDefaultModel | undefined, picked: string | undefined): string {
  if (picked !== undefined) return picked;
  if (view === undefined || !view.available) return '';
  return view.model_name;
}

/** Save waits for a choice that differs from an available stored value. */
function saveDisabled(view: PlatformDefaultModel | undefined, selected: string): boolean {
  return selected === '' || (view !== undefined && view.available && selected === view.model_name);
}

/**
 * The line a platform-model delete confirmation adds (#6826): who names the
 * model as a default, read before the operator confirms, because after the
 * delete each of them falls back.
 */
export function PlatformModelDeleteImpact({ modelId }: { readonly modelId: number }): ReactNode {
  const impact = platformDefaultImpact(usePlatformModelDefaultUsage(modelId).data);
  if (impact === undefined) return null;
  return (
    <Box component="span" sx={{ display: 'block' }} data-testid="platform-models-delete-default-impact">
      {impact}
    </Box>
  );
}

function PlatformDefaultAlerts({
  view,
  failure,
}: {
  readonly view: PlatformDefaultModel | undefined;
  readonly failure: Error | null;
}): ReactNode {
  return (
    <>
      {failure != null ? (
        <Alert severity="error" data-testid="platform-default-model-error">
          {configFailureReason(failure) ??
            t('pages.admin.platformDefault.error', 'The default model could not be read or saved.')}
        </Alert>
      ) : null}
      {view !== undefined && !view.available ? (
        <Alert severity="warning" data-testid="platform-default-model-unavailable">
          {t(
            'pages.admin.platformDefault.unavailable',
            '“{{name}}” is no longer available to every project. Choose a replacement.',
            { name: view.model_name },
          )}
        </Alert>
      ) : null}
    </>
  );
}

export function PlatformDefaultModelCard(): ReactNode {
  const { data, error } = useAdminPlatformDefaultModel();
  const save = useSavePlatformDefaultModel();
  const [picked, setPicked] = useState<string | undefined>(undefined);
  const selected = platformDefaultSelection(data, picked);
  const candidates = data?.candidates ?? [];

  const submit = (name: string | null) => {
    save.mutate(name, { onSuccess: () => setPicked(undefined) });
  };

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: '0.75rem' }} data-testid="platform-default-model">
      <Typography variant="headingSmall">
        {t('pages.admin.platformDefault.title', 'Default model')}
      </Typography>
      <Typography variant="bodySmall" color="text.secondary">
        {t(
          'pages.admin.platformDefault.intro',
          'New projects start with this model, and projects without their own default use it. A fast, low-cost model suits everyday chat.',
        )}
      </Typography>
      <PlatformDefaultAlerts view={data} failure={error ?? save.error} />
      <Box sx={{ display: 'flex', gap: '0.5rem', alignItems: 'center', flexWrap: 'wrap' }}>
        <TextField
          select
          size="small"
          sx={{ minWidth: '16rem' }}
          label={t('pages.admin.platformDefault.title', 'Default model')}
          value={selected}
          onChange={(event) => setPicked(event.target.value)}
          slotProps={{ htmlInput: { 'data-testid': 'platform-default-model-select' } }}
        >
          {candidates.map((candidate) => (
            <MenuItem key={candidate.name} value={candidate.name}>
              {candidate.display_name}
            </MenuItem>
          ))}
        </TextField>
        <Button
          variant="elitea"
          color="primary"
          size="small"
          data-testid="platform-default-model-save"
          disabled={save.isPending || saveDisabled(data, selected)}
          onClick={() => submit(selected)}
        >
          {t('common.save', 'Save')}
        </Button>
        <Button
          variant="elitea"
          color="tertiary"
          size="small"
          data-testid="platform-default-model-clear"
          disabled={save.isPending || (data?.model_name ?? '') === ''}
          onClick={() => submit(null)}
        >
          {t('pages.admin.platformDefault.clear', 'Clear')}
        </Button>
      </Box>
    </Box>
  );
}
