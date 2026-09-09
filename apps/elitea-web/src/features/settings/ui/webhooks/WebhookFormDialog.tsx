/**
 * WebhookFormDialog — create (or edit the url/events/active half of) one
 * outbound project webhook (#876).
 *
 * The secret itself is never editable through this form: creating always
 * mints a fresh one (`generateWebhookSecret`), and rotating an existing
 * webhook's secret is a separate one-click row action
 * (`WebhooksTable.tsx`'s "Rotate secret") — mixing the two into one form
 * would let a url/events edit accidentally also rotate the secret, or vice
 * versa, neither of which the five words on the row's menu promise.
 */
import { useCallback, useEffect, useState } from 'react';

import Box from '@mui/material/Box';
import FormControlLabel from '@mui/material/FormControlLabel';
import Switch from '@mui/material/Switch';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BaseModal } from '@/shared/ui/BaseModal';
import { InputBase } from '@/shared/ui/InputBase';

import { formatWebhookEvents, parseWebhookEvents } from '../../lib/webhooks/webhookHelpers';

export interface WebhookFormValues {
  readonly url: string;
  readonly events: readonly string[];
  readonly active: boolean;
}

export interface WebhookFormDialogProps {
  readonly open: boolean;
  readonly isSaving: boolean;
  /** Present when editing an existing webhook; absent for create. */
  readonly initialValues?: WebhookFormValues | undefined;
  readonly onClose: () => void;
  readonly onSubmit: (values: WebhookFormValues) => void;
}

const contentSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '1.25rem', minWidth: '25rem' };
const descriptionSx: SxProps<Theme> = { color: 'text.secondary' };

export function WebhookFormDialog({ open, isSaving, initialValues, onClose, onSubmit }: WebhookFormDialogProps) {
  const [url, setUrl] = useState('');
  const [events, setEvents] = useState('');
  const [active, setActive] = useState(true);
  const [urlError, setUrlError] = useState('');

  // Resets the form to the row being edited (or blank, for create) every time
  // the dialog opens — mirrors `PipelineWebhookModal`'s own open-triggered
  // reset, so a form left half-filled from a previous open never leaks in.
  useEffect(() => {
    if (!open) return;
    setUrl(initialValues?.url ?? '');
    setEvents(initialValues ? formatWebhookEvents(initialValues.events) : '');
    setActive(initialValues?.active ?? true);
    setUrlError('');
  }, [open, initialValues]);

  const handleSubmit = useCallback(() => {
    const trimmedUrl = url.trim();
    if (!trimmedUrl) {
      setUrlError(t('entities.webhook.form.urlRequired', 'A destination URL is required'));
      return;
    }
    onSubmit({ url: trimmedUrl, events: parseWebhookEvents(events), active });
  }, [url, events, active, onSubmit]);

  const isEdit = initialValues !== undefined;

  return (
    <BaseModal
      open={open}
      onClose={onClose}
      title={isEdit
        ? t('entities.webhook.form.editTitle', 'Edit webhook')
        : t('entities.webhook.form.createTitle', 'New webhook')}
      content={
        <Box sx={contentSx}>
          <Typography variant="bodyMedium" sx={descriptionSx}>
            {t(
              'entities.webhook.form.description',
              'Elitea POSTs a signed request to this URL whenever one of the listed events happens.',
            )}
          </Typography>
          <InputBase
            label={t('entities.webhook.form.urlLabel', 'Destination URL *')}
            placeholder="https://example.com/webhooks/elitea"
            value={url}
            onChange={(event) => {
              setUrl(event.target.value);
              setUrlError('');
            }}
            error={urlError !== ''}
            helperText={urlError || undefined}
            data-testid="webhook-form-url"
          />
          <InputBase
            label={t('entities.webhook.form.eventsLabel', 'Events')}
            placeholder="application.created, execution.completed"
            helperText={t(
              'entities.webhook.form.eventsHelp',
              'Comma-separated event types. Leave blank to fire on every event.',
            )}
            value={events}
            onChange={(event) => setEvents(event.target.value)}
            data-testid="webhook-form-events"
          />
          <FormControlLabel
            control={(
              <Switch
                checked={active}
                onChange={(event) => setActive(event.target.checked)}
                data-testid="webhook-form-active"
              />
            )}
            label={t('entities.webhook.form.activeLabel', 'Active')}
          />
        </Box>
      }
      actions={{
        confirming: isSaving,
        confirmText: isEdit ? t('entities.webhook.form.save', 'Save') : t('entities.webhook.form.create', 'Create'),
        cancelText: t('entities.webhook.form.cancel', 'Cancel'),
      }}
      onConfirm={handleSubmit}
      data-testid="webhook-form-dialog"
    />
  );
}
