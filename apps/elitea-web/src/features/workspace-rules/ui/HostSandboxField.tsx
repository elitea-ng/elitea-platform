/**
 * The rule form's "Without a sandbox" choice (ADR-0061 decision 5d,
 * `sandbox: host`): the person's own Allow rule may let the command run
 * outside the sandbox without a question, and only while the folder is
 * trusted. Never offered on a repository's rule: a repository can never
 * allow a host run, even edited back.
 */
import { FormControlLabel, Stack, Typography } from '@mui/material';

import type { RuleDecision } from '@/shared/desktop/rulesIpc';
import { t } from '@/shared/i18n';
import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';

export interface HostSandboxFieldProps {
  decision: RuleDecision;
  fromRepository: boolean;
  checked: boolean;
  onChange: (checked: boolean) => void;
}

export function HostSandboxField({ decision, fromRepository, checked, onChange }: HostSandboxFieldProps): React.JSX.Element | null {
  if (decision !== 'allow' || fromRepository) return null;
  return (
    <Stack spacing={0.5} data-testid="rule-host-sandbox">
      <FormControlLabel
        control={<BaseCheckbox checked={checked} onChange={(event: React.ChangeEvent<HTMLInputElement>) => onChange(event.target.checked)} />}
        label={t('workspace.rules.host', 'Without a sandbox, in a trusted folder (sandbox: host)')}
      />
      <Typography variant="bodySmall" color="text.secondary">
        {t('workspace.rules.hostHelp', 'In a trusted folder the command runs as you without asking. Elsewhere, or while trust is paused, the rule does nothing and the command is asked.')}
      </Typography>
    </Stack>
  );
}
