/**
 * The create/edit dialog for one LLM governance definition (#218).
 *
 * It renders the fields the chosen `type` actually has, not a JSON editor. The
 * reference design (design-governance-config-authoring §3.1) chose a
 * purpose-built routing editor over the raw-JSON fallback for exactly this
 * reason, and the same argument applies to every other type: an operator
 * authoring a budget should see a budget. The field groups live in
 * `./GovernanceDialogFields`.
 *
 * Every write is re-validated on the server; this dialog's inline hints are UX
 * only, and the server's refusal is what the operator is shown.
 */
import Alert from '@mui/material/Alert';
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import FormControlLabel from '@mui/material/FormControlLabel';
import MenuItem from '@mui/material/MenuItem';
import Switch from '@mui/material/Switch';
import TextField from '@mui/material/TextField';

import { t } from '@/shared/i18n';

import { GOVERNANCE_TYPES, isGlobalGovernanceType, type GovernanceType } from './api/adminGovernanceApi';
import { ScopeFields, TypeFields, typeHelp, typeLabel } from './GovernanceDialogFields';
import type { GovernanceDraft } from './useGatewayGovernancePage';

export interface GovernanceDialogProps {
  readonly draft: GovernanceDraft;
  readonly isNew: boolean;
  readonly isSaving: boolean;
  readonly saveError: string | undefined;
  readonly onChange: (patch: Partial<GovernanceDraft>) => void;
  readonly onCancel: () => void;
  readonly onSave: () => void;
}

export function GovernanceDialog({
  draft,
  isNew,
  isSaving,
  saveError,
  onChange,
  onCancel,
  onSave,
}: GovernanceDialogProps) {
  return (
    <Dialog open onClose={onCancel} fullWidth maxWidth="sm">
      <DialogTitle>
        {isNew
          ? t('pages.admin.governance.dialog.createTitle', 'New governance entry')
          : t('pages.admin.governance.dialog.editTitle', 'Edit governance entry')}
      </DialogTitle>
      <DialogContent
        dividers
        sx={{ display: 'flex', flexDirection: 'column', gap: '1rem', paddingTop: '1rem' }}
      >
        <TextField
          label={t('pages.admin.governance.field.name', 'Name')}
          helperText={t(
            'pages.admin.governance.field.nameHelp',
            'Unique for this type. It identifies the entry in the gateway’s logs and in its status report.',
          )}
          value={draft.name}
          onChange={(event) => onChange({ name: event.target.value })}
          size="small"
          fullWidth
          slotProps={{ htmlInput: { 'data-testid': 'governance-name' } }}
        />
        <TextField
          select
          label={t('pages.admin.governance.field.type', 'Type')}
          helperText={typeHelp(draft.type)}
          value={draft.type}
          // Changing the type is NOT allowed on an existing row: the row's
          // `data` holds the old type's document, the unique key is
          // (section, type, name), and a silent retype would leave the operator
          // looking at fields that were never saved. Delete and re-create.
          disabled={!isNew}
          onChange={(event) => onChange({ type: event.target.value as GovernanceType })}
          size="small"
          fullWidth
          slotProps={{ htmlInput: { 'data-testid': 'governance-type' } }}
        >
          {GOVERNANCE_TYPES.map((type) => (
            <MenuItem key={type} value={type}>
              {typeLabel(type)}
            </MenuItem>
          ))}
        </TextField>

        {/*
          A global type carries no scope. Showing the fields would invite the
          operator to author exactly the entry the server refuses, and the
          refusal would name a field they can no longer see on the next edit.
        */}
        {isGlobalGovernanceType(draft.type) ? null : <ScopeFields draft={draft} onChange={onChange} />}
        <TypeFields draft={draft} onChange={onChange} />

        <FormControlLabel
          control={
            <Switch
              checked={draft.enabled}
              onChange={(event) => onChange({ enabled: event.target.checked })}
              data-testid="governance-enabled"
            />
          }
          label={t('pages.admin.governance.field.enabled', 'Enabled')}
        />

        {saveError === undefined ? null : (
          <Alert severity="error" data-testid="governance-save-error">
            {saveError}
          </Alert>
        )}
      </DialogContent>
      <DialogActions>
        <Button onClick={onCancel} disabled={isSaving}>
          {t('common.cancel', 'Cancel')}
        </Button>
        <Button
          variant="contained"
          onClick={onSave}
          disabled={isSaving || draft.name.trim() === ''}
          data-testid="governance-save"
        >
          {t('common.save', 'Save')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}
