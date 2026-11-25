/**
 * Admin › Configuration › LLM Proxy — budget alerting.
 *
 * The global soft-alert control. `GET|PUT /admin/gateway/budget-alerts` have
 * existed since #322 with nothing calling them, so this setting has been
 * server-only: changeable with curl and invisible on every screen.
 *
 * ## What "global" means here, and what it does not
 *
 * The threshold is a DEFAULT. A project whose `gateway.project_budget` row
 * carries its own `soft_alert_pct` keeps using that one, so raising this number
 * does not raise every project's alert. The form says so, because "global"
 * would otherwise read as "applies to everything" and an operator would
 * conclude the per-project values had been overwritten.
 *
 * ## Disabling stops the alert, not the enforcement
 *
 * With alerting off the gateway still bills and still refuses a call that
 * exceeds a budget. Only the warning stops. That distinction is the one an
 * operator most needs before turning it off.
 */
import { useEffect, useState } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import FormControlLabel from '@mui/material/FormControlLabel';
import LinearProgress from '@mui/material/LinearProgress';
import Switch from '@mui/material/Switch';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import { ADMIN_ITEM } from './adminItemIds';
import { useAdminWritable } from './adminWriteAccess';
import { configFailureReason } from './api/adminConfigurationApi';
import { useBudgetAlertConfig, useSaveBudgetAlertConfig } from './api/adminBudgetAlertsApi';

/** The server's range for the threshold. Mirrored here as UX; the server decides. */
const MIN_THRESHOLD = 1;
const MAX_THRESHOLD = 100;

export function LlmProxyAlertsPanel() {
  const { data, isPending, error } = useBudgetAlertConfig();
  const access = useAdminWritable(ADMIN_ITEM.budgetsAlerts);
  const save = access.guard(useSaveBudgetAlertConfig());
  const locked = save.isPending || !access.writable;

  const [enabled, setEnabled] = useState(false);
  const [threshold, setThreshold] = useState('80');

  // Seeded from the server's answer, and re-seeded whenever it changes. The
  // form is not the source of truth for a setting that another admin may have
  // changed while this screen was open.
  useEffect(() => {
    if (data === undefined) return;
    setEnabled(data.enabled);
    setThreshold(String(data.threshold_pct));
  }, [data]);

  if (isPending) {
    return (
      <LinearProgress
        aria-label={t('pages.admin.llmProxy.alerts.loading', 'Loading alert config')}
      />
    );
  }

  if (error != null) {
    return (
      <Alert severity="warning" data-testid="llm-proxy-alerts-error">
        {configFailureReason(error) ??
          t('pages.admin.llmProxy.alerts.error', 'Failed to read the budget alert configuration.')}
      </Alert>
    );
  }

  const parsed = Number(threshold.trim());
  const thresholdValid =
    Number.isInteger(parsed) && parsed >= MIN_THRESHOLD && parsed <= MAX_THRESHOLD;

  return (
    <Box
      sx={{
        display: 'flex',
        flexDirection: 'column',
        gap: '1rem',
        maxWidth: '36rem',
      }}
    >
      <Typography variant="bodySmall" color="text.secondary">
        {t(
          'pages.admin.llmProxy.alerts.intro',
          'The gateway emits a soft alert when a budget reaches this share of its limit. Turning alerts off stops the warning only — budgets are still tracked and still enforced.',
        )}
      </Typography>

      {save.error != null ? (
        <Alert severity="error" data-testid="llm-proxy-alerts-save-error">
          {configFailureReason(save.error) ??
            t('pages.admin.llmProxy.alerts.saveError', 'Failed to save the alert configuration.')}
        </Alert>
      ) : null}

      <FormControlLabel
        control={
          <Switch
            checked={enabled}
            onChange={(event) => setEnabled(event.target.checked)}
            disabled={locked}
            data-testid="llm-proxy-alerts-enabled"
          />
        }
        label={t('pages.admin.llmProxy.alerts.enabled', 'Emit budget alerts')}
      />

      <TextField
        label={t('pages.admin.llmProxy.alerts.threshold', 'Default alert threshold (%)')}
        value={threshold}
        onChange={(event) => setThreshold(event.target.value)}
        disabled={locked}
        type="number"
        size="small"
        error={!thresholdValid}
        helperText={t(
          'pages.admin.llmProxy.alerts.thresholdHelp',
          'Applies to projects with no threshold of their own. A project that sets its own keeps it.',
        )}
        sx={{ maxWidth: '20rem' }}
        slotProps={{
          htmlInput: {
            'data-testid': 'llm-proxy-alerts-threshold',
            min: MIN_THRESHOLD,
            max: MAX_THRESHOLD,
          },
        }}
      />

      <Box>
        <Button
          variant="elitea" color="primary"
          disabled={!thresholdValid || locked}
          onClick={() => save.mutate({ enabled, threshold_pct: parsed })}
          data-testid="llm-proxy-alerts-save"
        >
          {t('pages.admin.llmProxy.alerts.save', 'Save')}
        </Button>
      </Box>
    </Box>
  );
}
