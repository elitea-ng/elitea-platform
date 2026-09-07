/**
 * Decide about one toolkit type, with a reason.
 *
 * Modelled on `./ServiceDescriptorDecisionDialog.tsx`, and for the same reason:
 * a decision an operator cannot justify is one the next operator cannot safely
 * reverse. The reason is required here, the submit button is disabled without
 * one, and the server's CHECK refuses it as well — so a client that forgot the
 * rule cannot write a row nobody can explain.
 *
 * ## Why `default` still asks for a reason box, and does not require it
 *
 * `default` DELETES the row. There is nothing left to carry a reason, so the
 * server ignores it and this dialog does not demand one. The field stays on
 * screen rather than disappearing, because a control that vanishes as the
 * operator changes a radio reads as a bug.
 *
 * ## One piece of state, not one boolean per availability
 *
 * `decision` is `null` or the type being decided. Two `open` booleans is how a
 * page ends up rendering both dialogs at once.
 */
import { useCallback, useEffect, useState } from 'react';

import Alert from '@mui/material/Alert';
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogContentText from '@mui/material/DialogContentText';
import DialogTitle from '@mui/material/DialogTitle';
import FormControlLabel from '@mui/material/FormControlLabel';
import Radio from '@mui/material/Radio';
import RadioGroup from '@mui/material/RadioGroup';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import {
  toolkitTypeFailureReason,
  useSaveToolkitTypePolicy,
  type AdminToolkitType,
  type ToolkitTypeAvailability,
} from './api/adminToolkitTypesApi';

export interface ToolkitTypeDecisionDialogProps {
  /** `null` closes it. */
  readonly toolkitType: AdminToolkitType | null;
  readonly onClose: () => void;
}

const CHOICES: readonly {
  readonly value: ToolkitTypeAvailability;
  readonly label: () => string;
  readonly help: () => string;
}[] = [
  {
    value: 'default',
    label: () => t('pages.admin.toolkitTypes.choice.default', 'Default'),
    help: () =>
      t(
        'pages.admin.toolkitTypes.choice.defaultHelp',
        'Remove the decision. The type is served as it ships, and its project exceptions are removed with it.',
      ),
  },
  {
    value: 'enabled',
    label: () => t('pages.admin.toolkitTypes.choice.enabled', 'Enabled'),
    help: () =>
      t('pages.admin.toolkitTypes.choice.enabledHelp', 'Offer the type in every project.'),
  },
  {
    value: 'disabled',
    label: () => t('pages.admin.toolkitTypes.choice.disabled', 'Disabled'),
    help: () =>
      t(
        'pages.admin.toolkitTypes.choice.disabledHelp',
        'Offer the type in no project. A project exception cannot bring it back.',
      ),
  },
  {
    value: 'restricted',
    label: () => t('pages.admin.toolkitTypes.choice.restricted', 'Restricted'),
    help: () =>
      t(
        'pages.admin.toolkitTypes.choice.restrictedHelp',
        'Offer the type only in the projects you grant it to.',
      ),
  },
];

export function ToolkitTypeDecisionDialog({
  toolkitType,
  onClose,
}: ToolkitTypeDecisionDialogProps) {
  const save = useSaveToolkitTypePolicy();

  const [availability, setAvailability] = useState<ToolkitTypeAvailability>('disabled');
  const [reason, setReason] = useState('');
  const [failure, setFailure] = useState<string | null>(null);

  // The dialog opens showing the CURRENT decision, so an operator who opens it
  // to read the state does not have to guess which radio is real.
  useEffect(() => {
    if (toolkitType === null) return;
    setAvailability(toolkitType.availability);
    setReason(toolkitType.reason);
    setFailure(null);
  }, [toolkitType]);

  const trimmed = reason.trim();
  const reverting = availability === 'default';
  const submittable = reverting || trimmed !== '';

  const close = useCallback(() => {
    setReason('');
    setFailure(null);
    onClose();
  }, [onClose]);

  const submit = useCallback(async () => {
    if (toolkitType === null || !submittable) return;
    setFailure(null);
    try {
      await save.mutateAsync({
        type: toolkitType.type,
        availability,
        reason: trimmed,
      });
      setReason('');
      onClose();
    } catch (error) {
      setFailure(
        toolkitTypeFailureReason(error) ??
          t(
            'pages.admin.toolkitTypes.error.write',
            'The toolkit type decision could not be saved.',
          ),
      );
    }
  }, [availability, onClose, save, submittable, toolkitType, trimmed]);

  return (
    <Dialog open={toolkitType !== null} onClose={close} fullWidth maxWidth="sm">
      <DialogTitle>{t('pages.admin.toolkitTypes.decide.title', 'Decide about a toolkit type')}</DialogTitle>
      <DialogContent>
        <DialogContentText sx={{ marginBottom: '1rem' }}>
          {t(
            'pages.admin.toolkitTypes.decide.body',
            'The decision changes which projects are offered this toolkit type. It never overrides the guardrails block list, which stays the last word.',
          )}
        </DialogContentText>
        {toolkitType === null ? null : (
          <Typography variant="body2" color="text.secondary" sx={{ marginBottom: '1rem' }}>
            {toolkitType.label} · {toolkitType.type}
          </Typography>
        )}
        <RadioGroup
          value={availability}
          onChange={(event) => setAvailability(event.target.value as ToolkitTypeAvailability)}
        >
          {CHOICES.map((choice) => (
            <FormControlLabel
              key={choice.value}
              value={choice.value}
              control={<Radio size="small" />}
              label={
                <span>
                  {choice.label()}
                  <Typography variant="caption" color="text.secondary" component="div">
                    {choice.help()}
                  </Typography>
                </span>
              }
            />
          ))}
        </RadioGroup>
        <TextField
          fullWidth
          required={!reverting}
          multiline
          minRows={2}
          sx={{ marginTop: '1rem' }}
          label={t('pages.admin.toolkitTypes.reason.label', 'Reason')}
          value={reason}
          onChange={(event) => setReason(event.target.value)}
        />
        {failure === null ? null : (
          <Alert severity="error" sx={{ marginTop: '1rem' }} data-testid="admin-toolkit-types-write-error">
            {failure}
          </Alert>
        )}
      </DialogContent>
      <DialogActions>
        <Button onClick={close} disabled={save.isPending}>
          {t('pages.admin.toolkitTypes.cancel', 'Cancel')}
        </Button>
        <Button
          variant="contained"
          disabled={!submittable || save.isPending}
          onClick={() => {
            void submit();
          }}
        >
          {t('pages.admin.toolkitTypes.action.save', 'Save decision')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}
