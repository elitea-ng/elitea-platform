/**
 * The one-screen "Credentials / Personal data" preset (issue 2012). It makes
 * the guardrail entries through the ordinary create route and closes on
 * success. A failure keeps the screen open with the server's reason, and names
 * the entries that were made before it.
 */
import { useState } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import FormControlLabel from '@mui/material/FormControlLabel';
import MenuItem from '@mui/material/MenuItem';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';

import { governanceFailureReason, useCreateGovernanceRow } from './api/adminGovernanceApi';
import {
  DEFAULT_PRESET,
  PRESET_ACTIONS,
  presetEntries,
  presetIsComplete,
  type GuardrailPresetChoice,
  type PresetAction,
} from './governanceGuardrailPreset';

function actionLabel(action: PresetAction): string {
  if (action === 'block') return t('pages.admin.governance.preset.actionBlock', 'Block the message');
  if (action === 'mask') return t('pages.admin.governance.preset.actionMask', 'Redact the match');
  return t('pages.admin.governance.preset.actionWarn', 'Warn, change nothing');
}

interface PackRowProps {
  readonly testId: string;
  readonly label: string;
  readonly help: string;
  readonly checked: boolean;
  readonly action: PresetAction;
  readonly onChecked: (checked: boolean) => void;
  readonly onAction: (action: PresetAction) => void;
}

function PackRow({ testId, label, help, checked, action, onChecked, onAction }: PackRowProps) {
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: '0.25rem' }}>
      <FormControlLabel
        control={<BaseCheckbox checked={checked} onChange={(event) => onChecked(event.target.checked)} />}
        label={label}
      />
      <Typography variant="bodySmall" color="text.secondary" sx={{ marginLeft: '2rem' }}>{help}</Typography>
      <TextField
        select
        size="small"
        disabled={!checked}
        value={action}
        onChange={(event) => onAction(event.target.value as PresetAction)}
        label={t('pages.admin.governance.preset.action', 'When it finds a match')}
        slotProps={{ htmlInput: { 'data-testid': `${testId}-action` } }}
        sx={{ marginLeft: '2rem' }}
      >
        {PRESET_ACTIONS.map((a) => <MenuItem key={a} value={a}>{actionLabel(a)}</MenuItem>)}
      </TextField>
    </Box>
  );
}

export interface GovernanceGuardrailPresetDialogProps {
  readonly onClose: () => void;
}

export function GovernanceGuardrailPresetDialog({ onClose }: GovernanceGuardrailPresetDialogProps) {
  const create = useCreateGovernanceRow();
  const [choice, setChoice] = useState<GuardrailPresetChoice>(DEFAULT_PRESET);
  const [error, setError] = useState<string | undefined>(undefined);
  const [saving, setSaving] = useState(false);
  // The entries made so far: a retry after a partial failure skips them.
  const [made, setMade] = useState<readonly string[]>([]);
  const patch = (change: Partial<GuardrailPresetChoice>): void => setChoice((current) => ({ ...current, ...change }));

  const onCreate = async (): Promise<void> => {
    setSaving(true);
    setError(undefined);
    const done = [...made];
    try {
      for (const entry of presetEntries(choice)) {
        if (done.includes(entry.name)) continue;
        await create.mutateAsync(entry);
        done.push(entry.name);
        setMade(done.slice());
      }
      onClose();
    } catch (failure) {
      const reason = governanceFailureReason(failure) ?? (failure instanceof Error ? failure.message : String(failure));
      setError(done.length === 0
        ? reason
        : t('pages.admin.governance.preset.partial', 'Created {{made}}, then failed: {{reason}}', { made: done.join(', '), reason }));
      setSaving(false);
    }
  };

  return (
    <Dialog open onClose={onClose} fullWidth maxWidth="sm" data-testid="governance-preset-dialog">
      <DialogTitle>{t('pages.admin.governance.preset.title', 'Credentials / Personal data')}</DialogTitle>
      <DialogContent dividers sx={{ display: 'flex', flexDirection: 'column', gap: '1rem', paddingTop: '1rem' }}>
        <Typography variant="bodySmall" color="text.secondary">
          {t('pages.admin.governance.preset.intro', 'Creates content guardrails with the gateway’s built-in rules. They apply to every project; edit an entry afterwards to narrow it. People see a notice in the chat when a message is blocked, redacted or warned about.')}
        </Typography>
        <TextField
          size="small"
          label={t('pages.admin.governance.field.name', 'Name')}
          value={choice.name}
          onChange={(event) => patch({ name: event.target.value })}
          slotProps={{ htmlInput: { 'data-testid': 'governance-preset-name' } }}
        />
        <PackRow
          testId="governance-preset-credentials"
          label={t('pages.admin.governance.preset.credentials', 'Credentials')}
          help={t('pages.admin.governance.preset.credentialsHelp', 'Cloud keys, access tokens, private keys, passwords in connection strings, JWTs.')}
          checked={choice.credentials}
          action={choice.credentialsAction}
          onChecked={(credentials) => patch({ credentials })}
          onAction={(credentialsAction) => patch({ credentialsAction })}
        />
        <PackRow
          testId="governance-preset-personal"
          label={t('pages.admin.governance.preset.personalData', 'Personal data')}
          help={t('pages.admin.governance.preset.personalDataHelp', 'E-mail addresses, phone numbers, payment cards (Luhn-checked) and IBANs (mod-97-checked).')}
          checked={choice.personalData}
          action={choice.personalDataAction}
          onChecked={(personalData) => patch({ personalData })}
          onAction={(personalDataAction) => patch({ personalDataAction })}
        />
        <Box>
          <FormControlLabel
            control={<BaseCheckbox checked={choice.input} onChange={(event) => patch({ input: event.target.checked })} />}
            label={t('pages.admin.governance.preset.input', 'Check what people send to the model')}
          />
          <FormControlLabel
            control={<BaseCheckbox checked={choice.output} onChange={(event) => patch({ output: event.target.checked })} />}
            label={t('pages.admin.governance.preset.output', 'Check the model’s answer')}
          />
        </Box>
        {error === undefined ? null : <Alert severity="error" data-testid="governance-preset-error">{error}</Alert>}
      </DialogContent>
      <DialogActions>
        <Button onClick={onClose} disabled={saving}>{t('common.cancel', 'Cancel')}</Button>
        <Button
          variant="contained"
          onClick={() => { void onCreate(); }}
          disabled={saving || !presetIsComplete(choice)}
          data-testid="governance-preset-create"
        >
          {t('pages.admin.governance.preset.create', 'Create')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}
