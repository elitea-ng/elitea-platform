/**
 * The rule form's grant and conditions (#3005–#3007), shown for an Allow
 * rule only: the login handed to the program, the hosts it may reach, and
 * the argument conditions that keep a write out of a read-only rule.
 */
import { FormControlLabel, MenuItem, Stack, TextField, Typography } from '@mui/material';

import { t } from '@/shared/i18n';
import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';

import type { GrantDraft } from '../model/grantText';
import { CREDENTIALS } from '../model/grantText';

export interface GrantFieldsProps {
  value: GrantDraft;
  onChange: (value: GrantDraft) => void;
}

export function GrantFields({ value, onChange }: GrantFieldsProps): React.JSX.Element {
  const text = (key: Exclude<keyof GrantDraft, 'graphqlQueryOnly'>) => (event: React.ChangeEvent<HTMLInputElement>) => onChange({ ...value, [key]: event.target.value });
  return (
    <Stack spacing={1.5} component="fieldset" data-testid="rule-grant" sx={{ border: 0, p: 0, m: 0 }}>
      <Typography component="legend" variant="labelMedium">
        {t('workspace.rules.grantTitle', 'Login and network (macOS)')}
      </Typography>
      <Typography variant="bodySmall" color="text.secondary">
        {t('workspace.rules.grantHelp', 'Let the program run with its stored login and reach only the hosts listed, still inside the sandbox. Only your own rules can do this; a repository’s never can. Elsewhere such commands are asked as before.')}
      </Typography>
      <TextField size="small" select label={t('workspace.rules.credential', 'Login')} value={value.credential} onChange={text('credential')}>
        <MenuItem value="">{t('workspace.rules.credentialNone', 'None')}</MenuItem>
        {CREDENTIALS.map((id) => (
          <MenuItem key={id} value={id}>
            {t('workspace.rules.credentialGh', 'The GitHub CLI (gh) login')}
          </MenuItem>
        ))}
      </TextField>
      <TextField size="small" label={t('workspace.rules.hosts', 'Hosts it may reach')} helperText={t('workspace.rules.hostsHelp', 'Host names separated by spaces, e.g. api.github.com; *.example.com for subdomains. Empty with a login: the login’s own hosts.')} value={value.hosts} onChange={text('hosts')} />
      <TextField size="small" label={t('workspace.rules.forbidFlags', 'Flags it must not have')} helperText={t('workspace.rules.forbidFlagsHelp', 'Anywhere in the command, e.g. -f -F --input')} value={value.forbidFlags} onChange={text('forbidFlags')} />
      <TextField size="small" label={t('workspace.rules.requireFlags', 'Flags it must have')} helperText={t('workspace.rules.requireFlagsHelp', 'e.g. --draft')} value={value.requireFlags} onChange={text('requireFlags')} />
      <TextField size="small" multiline minRows={2} label={t('workspace.rules.flagValues', 'Allowed flag values (one flag per line)')} helperText={t('workspace.rules.flagValuesHelp', 'The flag, then the values it may take, e.g. -X GET')} value={value.flagValues} onChange={text('flagValues')} />
      <FormControlLabel
        control={<BaseCheckbox size="small" checked={value.graphqlQueryOnly} onChange={(event) => onChange({ ...value, graphqlQueryOnly: event.target.checked })} />}
        label={t('workspace.rules.graphqlQuery', 'GraphQL queries only (no mutations)')}
      />
    </Stack>
  );
}
