/**
 * The upgrade policy of an installed agent (ADR-0051 decision 4): pinned,
 * notify or auto. Auto is low tier only and never takes a version that adds a
 * tool, a capability or a requirement, or raises the tier; the help line says so,
 * and says why when the newest version is waiting for a person.
 */
import { type ReactNode } from 'react';

import MenuItem from '@mui/material/MenuItem';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
import Box from '@mui/material/Box';

import { useMutation, useQueryClient } from '@tanstack/react-query';

import { setMarketplaceUpgradePolicy } from '@/shared/api/generated/marketplace/marketplace';
import type { MarketplaceOrigin } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import { invalidateMarketplace } from '../invalidate';

import { UPGRADE_POLICIES, policyLabel, readPolicy, wideningText, type UpgradePolicy } from './upgradeModel';

export interface UpgradePolicyControlProps {
  readonly origin: MarketplaceOrigin;
  readonly projectId: string;
  readonly applicationId: number;
}

export function UpgradePolicyControl({ origin, projectId, applicationId }: UpgradePolicyControlProps): ReactNode {
  const queryClient = useQueryClient();
  const current = readPolicy(origin.upgrade_policy);
  const save = useMutation({
    mutationFn: (policy: UpgradePolicy) => setMarketplaceUpgradePolicy(projectId, applicationId, { policy }),
    onSuccess: () => invalidateMarketplace(queryClient),
  });
  const waiting = origin.update_widening ?? [];
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: 0.5 }} data-testid="upgrade-policy">
      <TextField
        select
        size="small"
        label={t('marketplace.upgrade.policy.label', 'Upgrade policy')}
        value={current}
        disabled={save.isPending}
        onChange={(event) => save.mutate(readPolicy(event.target.value))}
        slotProps={{ htmlInput: { 'data-testid': 'upgrade-policy-input' } }}
        helperText={t(
          'marketplace.upgrade.policy.help',
          'Auto upgrades only at low tier, and never a version that adds a tool, a capability or a requirement, or raises the tier.',
        )}
      >
        {UPGRADE_POLICIES.map((policy) => (
          <MenuItem key={policy} value={policy}>
            {policyLabel(policy)}
          </MenuItem>
        ))}
      </TextField>
      {current === 'auto' && origin.latest_version != null && waiting.length > 0 && (
        <Typography component="output" variant="bodySmall" color="warning.main" data-testid="upgrade-auto-waiting">
          {t('marketplace.upgrade.autoWaiting', 'Auto upgrade is waiting for you: {{what}}', {
            what: waiting.map(wideningText).join('; '),
          })}
        </Typography>
      )}
      {save.isError && (
        <Typography variant="bodySmall" color="error.main" role="alert">
          {t('marketplace.upgrade.policy.failed', 'The policy was not saved. Try again.')}
        </Typography>
      )}
    </Box>
  );
}
