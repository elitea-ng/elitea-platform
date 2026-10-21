/**
 * The provider dialog's Authentication choice, for the types that can run
 * without a stored key (Vertex AI and DIAL).
 *
 * Its own file because it reads a fact about ANOTHER SERVICE: whether the LLM
 * gateway can mint workload-identity tokens at all. That is a Helm value, not a
 * row, so a credential saved in workload-identity mode on a gateway without it
 * is stored correctly and fails at every request. The warning says so while the
 * operator is still choosing.
 */
import type { ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import FormControl from '@mui/material/FormControl';
import FormControlLabel from '@mui/material/FormControlLabel';
import FormLabel from '@mui/material/FormLabel';
import Radio from '@mui/material/Radio';
import RadioGroup from '@mui/material/RadioGroup';

import { t } from '@/shared/i18n';

import { useGatewayStatus } from './api/adminLlmProxyApi';
import type { ProviderAuthMode } from './llmProviderForm';

export function LlmProviderAuthModeField({
  value,
  onChange,
}: {
  readonly value: ProviderAuthMode;
  readonly onChange: (mode: ProviderAuthMode) => void;
}): ReactNode {
  const status = useGatewayStatus();
  // Only an explicit `false` warns. A gateway that did not answer, or one too
  // old to report the field, is unknown, and unknown is not "off".
  const workloadIdentityOff =
    status.data?.reachable === true && status.data.gateway?.workload_identity_enabled === false;

  return (
    <FormControl>
      <FormLabel id="llm-provider-auth-mode-label">
        {t('pages.admin.llmProviders.field.authMode', 'Authentication')}
      </FormLabel>
      <RadioGroup
        aria-labelledby="llm-provider-auth-mode-label"
        value={value}
        onChange={(event) => {
          onChange(event.target.value === 'workload_identity' ? 'workload_identity' : 'key');
        }}
      >
        <FormControlLabel
          value="workload_identity"
          control={<Radio size="small" />}
          data-testid="llm-provider-auth-mode-workload-identity"
          label={t(
            'pages.admin.llmProviders.field.authModeWorkloadIdentity',
            'Workload identity (GKE service account)',
          )}
        />
        {workloadIdentityOff ? (
          <Alert severity="warning" data-testid="llm-provider-workload-identity-off">
            {t(
              'pages.admin.llmProviders.field.workloadIdentityOff',
              'The LLM gateway has workload identity turned off. The platform operator must set llmGateway.workloadIdentity.enabled in the Helm chart before this mode can work.',
            )}
          </Alert>
        ) : null}
        <FormControlLabel
          value="key"
          control={<Radio size="small" />}
          data-testid="llm-provider-auth-mode-key"
          label={t('pages.admin.llmProviders.field.authModeKey', 'Key')}
        />
      </RadioGroup>
    </FormControl>
  );
}
